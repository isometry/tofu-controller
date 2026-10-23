package polling

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/acl"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sLabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/flux-iac/tofu-controller/internal/config"
	bpconfig "github.com/flux-iac/tofu-controller/internal/config"
	"github.com/flux-iac/tofu-controller/internal/git/provider"

	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"
)

func (s *Server) getTerraformObject(ctx context.Context, ref client.ObjectKey) (*infrav1.Terraform, error) {
	obj := &infrav1.Terraform{}
	if err := s.clusterClient.Get(ctx, ref, obj); err != nil {
		return nil, fmt.Errorf("unable to get Terraform: %w", err)
	}

	return obj, nil
}

func (s *Server) listTerraformObjects(ctx context.Context, namespace string, labels map[string]string) ([]*infrav1.Terraform, error) {
	tfList := &infrav1.TerraformList{}

	opts := []client.ListOption{client.InNamespace(namespace)}

	if labels != nil {
		opts = append(opts, client.MatchingLabelsSelector{
			Selector: k8sLabels.Set(labels).AsSelector(),
		})
	}

	if err := s.clusterClient.List(ctx, tfList, opts...); err != nil {
		return nil, fmt.Errorf("unable to list Terraform objects: %w", err)
	}

	result := make([]*infrav1.Terraform, len(tfList.Items))
	for i := range tfList.Items {
		result[i] = &tfList.Items[i]
	}

	return result, nil
}

func (s *Server) getSource(ctx context.Context, tf *infrav1.Terraform) (*sourcev1.GitRepository, error) {
	if tf.Spec.SourceRef.Kind != sourcev1.GitRepositoryKind {
		return nil, fmt.Errorf("branch based planner does not support source kind: %s", tf.Spec.SourceRef.Kind)
	}

	ref := client.ObjectKey{
		Namespace: tf.GetNamespace(),
		Name:      tf.Spec.SourceRef.Name,
	}

	if ns := tf.Spec.SourceRef.Namespace; ns != "" {
		ref.Namespace = ns
	}

	if s.noCrossNamespaceRefs && ref.Namespace != tf.GetNamespace() {
		return nil, acl.AccessDeniedError(
			fmt.Sprintf("cannot access %s/%s, cross-namespace references have been disabled", tf.Spec.SourceRef.Kind, ref),
		)
	}

	obj := &sourcev1.GitRepository{}
	if err := s.clusterClient.Get(ctx, ref, obj); err != nil {
		return nil, fmt.Errorf("unable to get Source: %w", err)
	}

	return obj, nil
}

func (s *Server) reconcileTerraform(ctx context.Context, originalTF *infrav1.Terraform, originalSource *sourcev1.GitRepository, pr provider.PullRequest, prID string, interval time.Duration, gitProvider provider.Provider) error {
	tfName := config.PullRequestObjectName(originalTF.Name, prID)
	msg := fmt.Sprintf("Terraform object %s in the namespace %s", tfName, originalTF.Namespace)
	source, err := s.reconcileSource(ctx, originalTF.Name, originalSource, pr, prID, interval)
	if err != nil {
		return fmt.Errorf("unable to reconcile Source for %s: %w", msg, err)
	}

	fileMappings, err := s.reconcileFileMappings(ctx, originalTF, pr, prID, gitProvider)
	if err != nil {
		return fmt.Errorf("unable to reconcile file mapping Secrets for %s: %w", msg, err)
	}

	tf := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tfName,
			Namespace: originalTF.Namespace,
		},
	}

	branchLabels := s.createLabels(originalTF.Labels, originalTF.Name, pr.HeadBranch, prID)
	op, err := controllerutil.CreateOrUpdate(ctx, s.clusterClient, tf, func() error {
		spec := originalTF.Spec.DeepCopy()

		spec.SourceRef.Name = source.Name
		spec.SourceRef.Namespace = source.Namespace

		// DestroyResourcesOnDeletion must be false, otherwise plan deletion will destroy resources
		spec.DestroyResourcesOnDeletion = false
		spec.PlanOnly = true
		spec.StoreReadablePlan = "human"

		// We don't need to examine or use the outputs of the plan
		spec.WriteOutputsToSecret = nil

		spec.ApprovePlan = ""
		spec.Force = false

		// Support branch planning for Terraform Cloud
		// By using local state and a local backend for the branch plan object
		if spec.Cloud != nil || spec.CliConfigSecretRef != nil {
			spec.Cloud = nil
			spec.CliConfigSecretRef = nil
		}

		// Use the files of the pull request
		spec.FileMappings = fileMappings.fileMappings
		spec.TfVarsFiles = fileMappings.tfVarsFiles

		if spec.BackendConfig == nil {
			spec.BackendConfig = &infrav1.BackendConfigSpec{
				SecretSuffix:    originalTF.Name,
				InClusterConfig: true,
			}
		}

		tf.Spec = *spec

		tf.SetLabels(branchLabels)

		return nil
	})
	if err != nil {
		return fmt.Errorf("reconcile failed for %s: %w", msg, err)
	} else if op != controllerutil.OperationResultNone {
		s.log.Info(fmt.Sprintf("%s successfully reconciled", msg), "operation", op)
	}

	// The Terraform controller only plans again when the source revision
	// changes. If the head commit was already planned before the file
	// mappings caught up with it, the plan used outdated files and has to be
	// redone.
	fileMappingsChanged := fileMappings.changed || op == controllerutil.OperationResultUpdated
	if fileMappingsChanged && pr.HeadSha != "" && strings.HasSuffix(tf.Status.LastPlannedRevision, pr.HeadSha) {
		s.log.Info(fmt.Sprintf("file mappings of %s changed after planning, triggering replan", msg))
		if err := s.replanTerraform(ctx, tf); err != nil {
			return fmt.Errorf("unable to trigger replan for %s: %w", msg, err)
		}
	}

	return nil
}

type prFileMappings struct {
	// fileMappings and tfVarsFiles are the spec fields of the pull request
	// Terraform object.
	fileMappings []infrav1.FileMapping
	tfVarsFiles  []string
	// changed is true if any of the Secret copies has been updated.
	changed bool
}

type prFileMappingSecret struct {
	original   *corev1.Secret
	copyName   string
	data       map[string][]byte
	candidates []string
}

// reconcileFileMappings creates a copy of each Secret referenced by a
// workspace file mapping, with the content of its files in the pull request,
// and returns the file mappings and tfvars files using them. Workspace file
// mappings are written on top of the source, so without this, files kept in
// both the repository and a Secret (e.g. tfvars) would always have the
// content of the tracked branch.
//
// The files of a Secret are looked up in the repository directory set by its
// AnnotationFileMappingSourceDir annotation, or where the file mapping writes
// them otherwise. If the Secret has the AnnotationFileMappingSourceFiles
// annotation, its candidate files that exist in the pull request are mapped
// and loaded as tfvars files in the given order, so files added or deleted by
// the pull request are taken into account. Otherwise, only the mapped files
// are looked up: files deleted by the pull request are dropped, and files
// only in the Secret keep its content.
func (s *Server) reconcileFileMappings(ctx context.Context, originalTF *infrav1.Terraform, pr provider.PullRequest, prID string, gitProvider provider.Provider) (prFileMappings, error) {
	result := prFileMappings{}

	mappingsBySecret := map[string][]infrav1.FileMapping{}
	for _, fileMapping := range originalTF.Spec.FileMappings {
		if fileMapping.Location == "workspace" {
			mappingsBySecret[fileMapping.SecretRef.Name] = append(mappingsBySecret[fileMapping.SecretRef.Name], fileMapping)
		}
	}

	secrets := map[string]*prFileMappingSecret{}
	for secretName, fileMappings := range mappingsBySecret {
		secret, changed, err := s.reconcileFileMappingSecret(ctx, originalTF, secretName, fileMappings, pr, prID, gitProvider)
		if err != nil {
			return result, err
		}

		secrets[secretName] = secret
		result.changed = result.changed || changed
	}

	workspacePath := func(fileMapping infrav1.FileMapping) string {
		return path.Join(originalTF.Spec.Path, fileMapping.Path)
	}

	originalVarsFiles := map[string]bool{}
	for _, varsFile := range originalTF.Spec.TfVarsFiles {
		originalVarsFiles[path.Clean(varsFile)] = true
	}

	// Paths of the tfvars files of the Secret copies, which replace the
	// tfvars files of the original Secrets.
	managedVarsFiles := map[string]bool{}
	var varsFiles []string
	candidatesAdded := map[string]bool{}

	for _, fileMapping := range originalTF.Spec.FileMappings {
		secret, ok := secrets[fileMapping.SecretRef.Name]
		if !ok || fileMapping.Location != "workspace" {
			result.fileMappings = append(result.fileMappings, fileMapping)
			continue
		}

		managedVarsFiles[workspacePath(fileMapping)] = true
		key := fileMapping.SecretRef.Key

		if slices.Contains(secret.candidates, key) {
			// The candidate files replace all the candidate mappings, in the
			// position of the first one.
			if candidatesAdded[fileMapping.SecretRef.Name] {
				continue
			}
			candidatesAdded[fileMapping.SecretRef.Name] = true

			for _, candidate := range secret.candidates {
				if _, ok := secret.data[candidate]; !ok {
					continue
				}

				candidateMapping := infrav1.FileMapping{
					Location:  "workspace",
					Path:      candidate,
					SecretRef: meta.SecretKeyReference{Name: secret.copyName, Key: candidate},
				}
				result.fileMappings = append(result.fileMappings, candidateMapping)
				varsFiles = append(varsFiles, workspacePath(candidateMapping))
			}

			continue
		}

		// Drop the files deleted by the pull request
		if _, inOriginal := secret.original.Data[key]; inOriginal {
			if _, inCopy := secret.data[key]; !inCopy {
				continue
			}
		}

		if originalVarsFiles[workspacePath(fileMapping)] {
			varsFiles = append(varsFiles, workspacePath(fileMapping))
		}

		fileMapping.SecretRef.Name = secret.copyName
		result.fileMappings = append(result.fileMappings, fileMapping)
	}

	// The tfvars files of the Secret copies replace the original ones, in the
	// position of the first one to keep the precedence.
	inserted := false
	for _, varsFile := range originalTF.Spec.TfVarsFiles {
		if !managedVarsFiles[path.Clean(varsFile)] {
			result.tfVarsFiles = append(result.tfVarsFiles, varsFile)
			continue
		}

		if !inserted {
			result.tfVarsFiles = append(result.tfVarsFiles, varsFiles...)
			inserted = true
		}
	}
	if !inserted {
		result.tfVarsFiles = append(result.tfVarsFiles, varsFiles...)
	}

	return result, nil
}

// reconcileFileMappingSecret creates or updates the copy of a Secret with the
// content of its files in the pull request. The files are fetched again only
// when the head of the pull request, the original Secret or the original
// Terraform object change, to not exhaust the rate limit of the git provider.
func (s *Server) reconcileFileMappingSecret(ctx context.Context, originalTF *infrav1.Terraform, secretName string, fileMappings []infrav1.FileMapping, pr provider.PullRequest, prID string, gitProvider provider.Provider) (*prFileMappingSecret, bool, error) {
	original, err := s.getSecret(ctx, client.ObjectKey{Namespace: originalTF.Namespace, Name: secretName})
	if err != nil {
		return nil, false, err
	}

	result := &prFileMappingSecret{
		original: original,
		copyName: config.FileMappingSecretName(originalTF.Name, secretName, prID),
	}

	if value, ok := original.Annotations[bpconfig.AnnotationFileMappingSourceFiles]; ok {
		if err := json.Unmarshal([]byte(value), &result.candidates); err != nil {
			return nil, false, fmt.Errorf("invalid annotation %s of Secret %s in the namespace %s: %w", bpconfig.AnnotationFileMappingSourceFiles, original.Name, original.Namespace, err)
		}
	}

	headRef := cmp.Or(pr.HeadSha, pr.HeadBranch)
	revision := fmt.Sprintf("%s/%s/%d", headRef, original.ResourceVersion, originalTF.Generation)

	secret := &corev1.Secret{}
	err = s.clusterClient.Get(ctx, client.ObjectKey{Namespace: originalTF.Namespace, Name: result.copyName}, secret)
	if err == nil && pr.HeadSha != "" && secret.Annotations[bpconfig.AnnotationFileMappingRevision] == revision {
		result.data = secret.Data
		return result, false, nil
	}
	if client.IgnoreNotFound(err) != nil {
		return nil, false, fmt.Errorf("unable to get Secret %s in the namespace %s: %w", result.copyName, originalTF.Namespace, err)
	}

	result.data, err = s.fetchFileMappingFiles(ctx, originalTF, result, fileMappings, pr, gitProvider)
	if err != nil {
		return nil, false, err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      result.copyName,
			Namespace: originalTF.Namespace,
		},
	}
	branchLabels := s.createLabels(nil, originalTF.Name, pr.HeadBranch, prID)

	var previousData map[string][]byte
	op, err := controllerutil.CreateOrUpdate(ctx, s.clusterClient, secret, func() error {
		previousData = secret.Data
		secret.SetLabels(branchLabels)
		secret.SetAnnotations(map[string]string{bpconfig.AnnotationFileMappingRevision: revision})
		secret.Data = result.data

		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("reconcile failed for Secret %s in the namespace %s: %w", secret.Name, secret.Namespace, err)
	} else if op != controllerutil.OperationResultNone {
		s.log.Info(fmt.Sprintf("Secret %s in the namespace %s successfully reconciled", secret.Name, secret.Namespace), "operation", op)
	}

	// Only the content matters, not the revision annotation.
	changed := op == controllerutil.OperationResultUpdated && !maps.EqualFunc(previousData, result.data, bytes.Equal)

	return result, changed, nil
}

// fetchFileMappingFiles returns the content of the original Secret with its
// files replaced by their content in the pull request. Files deleted by the
// pull request, and candidate files not in the pull request, are removed.
func (s *Server) fetchFileMappingFiles(ctx context.Context, originalTF *infrav1.Terraform, secret *prFileMappingSecret, fileMappings []infrav1.FileMapping, pr provider.PullRequest, gitProvider provider.Provider) (map[string][]byte, error) {
	headRef := cmp.Or(pr.HeadSha, pr.HeadBranch)
	baseRef := cmp.Or(pr.BaseSha, pr.BaseBranch)

	data := maps.Clone(secret.original.Data)
	if data == nil {
		data = map[string][]byte{}
	}

	sourceDir, hasSourceDir := secret.original.Annotations[bpconfig.AnnotationFileMappingSourceDir]
	repoPath := func(key, mappingPath string) (string, bool) {
		p := path.Join(originalTF.Spec.Path, mappingPath)
		if hasSourceDir {
			p = path.Join(sourceDir, key)
		}
		p = strings.TrimPrefix(p, "/")

		return p, p != ".." && !strings.HasPrefix(p, "../")
	}

	for _, candidate := range secret.candidates {
		p, ok := repoPath(candidate, candidate)
		if !ok {
			return nil, fmt.Errorf("candidate file %s is outside the repository", candidate)
		}

		content, err := gitProvider.GetFileContent(ctx, pr.Repository, p, headRef)
		if errors.Is(err, provider.ErrFileNotFound) {
			delete(data, candidate)
			continue
		}
		if err != nil {
			return nil, err
		}

		data[candidate] = content
	}

	for _, fileMapping := range fileMappings {
		key := fileMapping.SecretRef.Key
		if slices.Contains(secret.candidates, key) {
			continue
		}

		p, ok := repoPath(key, fileMapping.Path)
		if !ok {
			continue
		}

		content, err := gitProvider.GetFileContent(ctx, pr.Repository, p, headRef)
		if err == nil {
			data[key] = content
			continue
		}
		if !errors.Is(err, provider.ErrFileNotFound) {
			return nil, err
		}

		// The file is not in the pull request. If it is in the base branch,
		// the pull request deletes it. Otherwise, it is only in the Secret
		// and its content is kept.
		_, err = gitProvider.GetFileContent(ctx, pr.Repository, p, baseRef)
		if errors.Is(err, provider.ErrFileNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}

		s.log.Info("file deleted by the pull request, dropping its file mapping", "path", p, "PR ID", pr.Number)
		delete(data, key)
	}

	return data, nil
}

func (s *Server) reconcileSource(ctx context.Context, tfName string, originalSource *sourcev1.GitRepository, pr provider.PullRequest, prID string, interval time.Duration) (*sourcev1.GitRepository, error) {
	sourceName := config.SourceName(tfName, originalSource.Name, prID)
	msg := fmt.Sprintf("Source %s in the namespace %s", sourceName, originalSource.Namespace)
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sourceName,
			Namespace: originalSource.Namespace,
		},
		Spec: originalSource.Spec,
	}
	branchLabels := s.createLabels(originalSource.Labels, originalSource.Name, pr.HeadBranch, prID)

	op, err := controllerutil.CreateOrUpdate(ctx, s.clusterClient, source, func() error {
		source.SetLabels(branchLabels)

		spec := originalSource.Spec.DeepCopy()

		spec.Reference = &sourcev1.GitRepositoryRef{Branch: pr.HeadBranch}
		// Flux cannot resolve abbreviated SHAs (e.g. Bitbucket Cloud returns 12 chars); fall back to the branch tip.
		if len(pr.HeadSha) == 40 || len(pr.HeadSha) == 64 {
			spec.Reference.Commit = pr.HeadSha
		}
		spec.Interval = metav1.Duration{
			Duration: interval,
		}

		source.Spec = *spec

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile failed for %s: %w", msg, err)
	} else if op != controllerutil.OperationResultNone {
		s.log.Info(fmt.Sprintf("%s successfully reconciled", msg), "operation", op)
	}

	return source, nil
}

func (s *Server) createLabels(labels map[string]string, originalName string, branch string, prID string) map[string]string {
	resultLabels := make(map[string]string)
	maps.Copy(resultLabels, labels)

	resultLabels[bpconfig.LabelKey] = bpconfig.LabelValue
	resultLabels[bpconfig.LabelPrimaryResourceKey] = originalName
	resultLabels[bpconfig.LabelPRIDKey] = prID
	return resultLabels
}

func (s *Server) deleteTerraformAndSource(ctx context.Context, tf *infrav1.Terraform) error {
	const (
		pollInterval = 5 * time.Second // Interval to check the deletion status
		pollTimeout  = 2 * time.Minute // Total time before timing out
	)

	tfMsg := fmt.Sprintf("Terraform %s in the namespace %s", tf.Name, tf.Namespace)

	// Get source, but not yet delete it
	source, err := s.getSource(ctx, tf)
	if err != nil {
		return fmt.Errorf("unable to get Source for %s: %w", tfMsg, err)
	}

	// Delete Terraform object
	if err := s.clusterClient.Delete(ctx, tf); err != nil {
		return fmt.Errorf("unable to delete %s: %w", tfMsg, err)
	}

	// We have to wait for the Terraform object to be deleted before deleting the source
	err = wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, func(ctx context.Context) (bool, error) {
		if err := s.clusterClient.Get(ctx, client.ObjectKey{Name: tf.Name, Namespace: tf.Namespace}, tf); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil // Terraform object is deleted
			}
			return false, err // An error occurred
		}
		return false, nil // Terraform object still exists
	})

	if err != nil {
		return fmt.Errorf("error waiting for %s to be deleted: %w", tfMsg, err)
	}

	s.log.Info(fmt.Sprintf("deleted %s", tfMsg))

	sourceMsg := fmt.Sprintf("Source %s in the namespace %s", source.Name, source.Namespace)
	if err := s.clusterClient.Delete(ctx, source); err != nil {
		return fmt.Errorf("unable to delete %s: %w", sourceMsg, err)
	}

	s.log.Info(fmt.Sprintf("deleted %s", sourceMsg))

	return s.deleteFileMappingSecrets(ctx, tf)
}

func (s *Server) deleteFileMappingSecrets(ctx context.Context, tf *infrav1.Terraform) error {
	secrets := &corev1.SecretList{}
	if err := s.clusterClient.List(ctx, secrets, client.InNamespace(tf.Namespace), client.MatchingLabels{
		bpconfig.LabelKey:                bpconfig.LabelValue,
		bpconfig.LabelPrimaryResourceKey: tf.Labels[bpconfig.LabelPrimaryResourceKey],
		bpconfig.LabelPRIDKey:            tf.Labels[bpconfig.LabelPRIDKey],
	}); err != nil {
		return fmt.Errorf("unable to list file mapping Secrets of Terraform %s in the namespace %s: %w", tf.Name, tf.Namespace, err)
	}

	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if err := s.clusterClient.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("unable to delete Secret %s in the namespace %s: %w", secret.Name, secret.Namespace, err)
		}

		s.log.Info(fmt.Sprintf("deleted Secret %s in the namespace %s", secret.Name, secret.Namespace))
	}

	return nil
}

func (s *Server) getSecret(ctx context.Context, ref client.ObjectKey) (*corev1.Secret, error) {
	obj := &corev1.Secret{}
	if err := s.clusterClient.Get(ctx, ref, obj); err != nil {
		return nil, fmt.Errorf("unable to get Secret: %w", err)
	}

	return obj, nil
}
