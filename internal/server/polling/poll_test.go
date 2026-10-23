package polling

import (
	"context"
	"testing"

	"github.com/flux-iac/tofu-controller/internal/config"
	bpconfig "github.com/flux-iac/tofu-controller/internal/config"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/flux-iac/tofu-controller/internal/git/provider/providerfakes"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/flux-iac/tofu-controller/api/v1alpha2"
	"github.com/flux-iac/tofu-controller/internal/git/provider"
	"github.com/fluxcd/pkg/git"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
)

// This checks poll can be called with a little setting-up, with no
// result expected.
func Test_poll_empty(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)

	// Create a source for the Terraform object to point to
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/flux-iac/tofu-controller",
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(t.Context(), source))

	// Create a Terraform object to be the template.
	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
		},
		Spec: infrav1.TerraformSpec{
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(t.Context(), original))

	// This fakes a provider for the server to use.
	var prs []provider.PullRequest

	server, err := New(
		WithClusterClient(k8sClient),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Now we'll run `reconcile` to step the server once, and afterwards,
	// we should be able to see what it did.
	ctx := t.Context()
	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, &providerfakes.FakeProvider{}))

	// We expect it to have done nothing! So, check it didn't create
	// any more Terraform or source objects.
	var tfList infrav1.TerraformList
	expectToSucceed(t, g, k8sClient.List(t.Context(), &tfList, &client.ListOptions{
		Namespace: ns.Name,
	}))
	expectToEqual(t, g, len(tfList.Items), 1) // just the original
	expectToEqual(t, g, tfList.Items[0].Name, original.Name)

	var srcList sourcev1.GitRepositoryList
	expectToSucceed(t, g, k8sClient.List(t.Context(), &srcList, &client.ListOptions{
		Namespace: ns.Name,
	}))
	expectToEqual(t, g, len(srcList.Items), 1) // just `source`
	expectToEqual(t, g, srcList.Items[0].Name, source.Name)

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}

// This checks that branch Terraform objects are created,
// when there are open pull requests,
// updated when the original Terraform object is updated,
// and deleted when the corresponding PRs are closed.
// The original Terraform object and source should be retained.
func Test_poll_reconcile_objects(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)

	// Create a source for the Terraform object to point to
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
			Labels: map[string]string{
				"test-label": "123",
			},
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/tf-controller/helloworld",
			// Extra ref fields must not be inherited by the PR sources.
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
				Name:   "refs/heads/main",
				Commit: "0000000000000000000000000000000000000000",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(t.Context(), source))

	// Create a Terraform object to be the template.
	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
			Labels: map[string]string{
				"test-label": "abc",
			},
		},
		Spec: infrav1.TerraformSpec{
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
			WriteOutputsToSecret: &infrav1.WriteOutputsToSecretSpec{
				Name: "test-secret",
			},
			ApprovePlan: "should be cleared",
			Force:       true, // should be set false on clone.
		},
	}
	expectToSucceed(t, g, k8sClient.Create(t.Context(), original))

	// This fakes a provider for the server to use.
	repo := provider.Repository{
		Project: "fake-project",
		Org:     "fake-org",
		Name:    "fake-name",
	}
	fullSha := "1111111111111111111111111111111111111111"
	prs := []provider.PullRequest{
		{
			Repository: repo,
			Number:     1,
			BaseBranch: "main",
			HeadBranch: "test-branch-1",
			HeadSha:    fullSha,
		},
		{
			Repository: repo,
			Number:     2,
			BaseBranch: "main",
			HeadBranch: "test-branch-2",
			HeadSha:    "222222222222", // abbreviated, so not pinned
		},
		{
			Repository: repo,
			Number:     3,
			BaseBranch: "main",
			HeadBranch: "test-branch-3",
		},
		{
			// Targets another branch than the source tracks, so it's not planned.
			Repository: repo,
			Number:     4,
			BaseBranch: "develop",
			HeadBranch: "test-branch-4",
		},
	}

	server, err := New(
		WithClusterClient(k8sClient),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Now we'll run `reconcile` to step the server once, and afterwards,
	// we should be able to see what it did.
	ctx := t.Context()
	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, &providerfakes.FakeProvider{}))

	// We expect the branch TF objects and corresponding sources
	// to be created for each PR
	// and the original object and source to be retained.

	// Check that the Terraform objects are created with expected fields.
	var tfList infrav1.TerraformList
	expectToSucceed(t, g, k8sClient.List(t.Context(), &tfList, &client.ListOptions{
		Namespace: ns.Name,
	}))

	expectToEqual(t, g, len(tfList.Items), 4)
	for _, item := range tfList.Items {
		g.Expect(item.Labels[bpconfig.LabelPRIDKey]).NotTo(gomega.Equal("4"))
	}
	for _, item := range tfList.Items {
		if item.Name == original.Name {
			// Ignore the original source object.
			continue
		}
		prID := item.Labels[bpconfig.LabelPRIDKey]
		expectToEqual(t, g, item.Name, config.PullRequestObjectName(original.Name, prID))
		expectToEqual(t, g, item.Spec.SourceRef.Name, config.SourceName(original.Name, source.Name, prID))
		expectToEqual(t, g, item.Spec.SourceRef.Namespace, ns.Name)
		expectToEqual(t, g, item.Spec.PlanOnly, true)
		expectToEqual(t, g, item.Spec.StoreReadablePlan, "human")
		expectToEqual(t, g, item.Spec.ApprovePlan, "")
		expectToEqual(t, g, item.Spec.Force, false)
		g.Expect(item.Spec.WriteOutputsToSecret).To(gomega.BeNil()) // we don't need to use the output Secret of the plan
		expectToEqual(t, g, item.Labels[bpconfig.LabelKey], bpconfig.LabelValue)
		expectToEqual(t, g, item.Labels["test-label"], "abc")
		expectToEqual(t, g, item.Spec.BackendConfig.SecretSuffix, original.Name)
		expectToEqual(t, g, item.Spec.BackendConfig.InClusterConfig, true)
	}

	// Check that the Source objects are created with all expected fields.
	var srcList sourcev1.GitRepositoryList
	expectToSucceed(t, g, k8sClient.List(t.Context(), &srcList, &client.ListOptions{
		Namespace: ns.Name,
	}))

	expectToEqual(t, g, len(srcList.Items), 4)
	for _, item := range srcList.Items {
		if item.Name == source.Name {
			// Ignore the original source object.
			continue
		}
		prID := item.Labels[bpconfig.LabelPRIDKey]
		expectToEqual(t, g, item.Name, config.SourceName(original.Name, source.Name, prID))
		expectedRef := &sourcev1.GitRepositoryRef{Branch: "test-branch-" + prID}
		if prID == "1" {
			expectedRef.Commit = fullSha
		}
		expectToEqual(t, g, item.Spec.Reference, expectedRef)
		expectToEqual(t, g, item.Labels[bpconfig.LabelKey], bpconfig.LabelValue)
		expectToEqual(t, g, item.Labels[bpconfig.LabelPRIDKey], prID)
		expectToEqual(t, g, item.Labels["test-label"], "123")
	}

	// Check that branch Terraform objects are updated
	// after the original Terraform object is updated.
	secretName := "new-test-secret"
	original.Labels["test-label"] = "xyz"
	original.Spec.WriteOutputsToSecret.Name = secretName

	expectToSucceed(t, g, k8sClient.Update(t.Context(), original))
	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, &providerfakes.FakeProvider{}))

	tfList.Items = nil

	expectToSucceed(t, g, k8sClient.List(t.Context(), &tfList, &client.ListOptions{
		Namespace:     ns.Name,
		LabelSelector: labels.Set{bpconfig.LabelKey: bpconfig.LabelValue}.AsSelector(),
	}))

	for _, item := range tfList.Items {
		expectToEqual(t, g, item.Labels["test-label"], "xyz")
		g.Expect(item.Spec.WriteOutputsToSecret).To(gomega.BeNil())
	}

	// Check that corresponding Terraform objects and Sources are deleted
	// after PRs are deleted
	// and the original Terraform object and source are retained.
	prs = prs[2:]

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, &providerfakes.FakeProvider{}))

	tfList.Items = nil

	expectToSucceed(t, g, k8sClient.List(t.Context(), &tfList, &client.ListOptions{
		Namespace: ns.Name,
	}))

	expectToEqual(t, g, len(tfList.Items), 2)
	expectToEqual(t, g, tfList.Items[0].Name, original.Name)
	expectToEqual(t, g, tfList.Items[1].Name, original.Name+"-pr-3")

	srcList.Items = nil

	expectToSucceed(t, g, k8sClient.List(t.Context(), &srcList, &client.ListOptions{
		Namespace: ns.Name,
	}))

	expectToEqual(t, g, len(srcList.Items), 2)
	for _, item := range srcList.Items {
		if item.Name == source.Name {
			continue
		}
		// Only one item left and it should be PR#3.
		expectToEqual(t, g, item.Name, config.SourceName(original.Name, source.Name, "3"))
	}

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}

// If there are no Terraform changes in a Pull Request, and
// `.spec.BranchPlanner.EnablePathScope` is true, we expect no new resources are
// created for that Pull Request.
func Test_poll_noPathChanges(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)

	// Create a source for the Terraform object to point to
	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/flux-iac/tofu-controller",
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(t.Context(), source))

	// Create a Terraform object to be the template.
	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
		},
		Spec: infrav1.TerraformSpec{
			Path: "./infra/",
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
			BranchPlanner: &infrav1.BranchPlanner{
				EnablePathScope: true,
			},
			BackendConfig: &infrav1.BackendConfigSpec{
				SecretSuffix: "special-value",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(t.Context(), original))

	repo := provider.Repository{
		Project: "fake-project",
		Org:     "fake-org",
		Name:    "fake-name",
	}
	prs := []provider.PullRequest{
		{
			Repository: repo,
			Number:     1,
			BaseBranch: "main",
			HeadBranch: "test-branch-1",
		},
	}
	prChanges := []provider.Change{
		{
			Path:      "cmd/project/main.go",
			Additions: 2,
			Added:     true,
		},
	}

	gitProvider := &providerfakes.FakeProvider{}
	gitProvider.ListPullRequestChangesReturns(prChanges, nil)

	server, err := New(
		WithClusterClient(k8sClient),
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	// Now we'll run `reconcile` to step the server once, and afterwards,
	// we should be able to see what it did.
	ctx := t.Context()
	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, gitProvider))

	// We expect it to have done nothing! So, check it didn't create
	// any more Terraform or source objects.
	var tfList infrav1.TerraformList
	expectToSucceed(t, g, k8sClient.List(t.Context(), &tfList, &client.ListOptions{
		Namespace: ns.Name,
	}))
	expectToEqual(t, g, len(tfList.Items), 1, "terraform list") // just the original
	expectToEqual(t, g, tfList.Items[0].Name, original.Name)
	expectToEqual(t, g, tfList.Items[0].Spec.BackendConfig, original.Spec.BackendConfig)

	var srcList sourcev1.GitRepositoryList
	expectToSucceed(t, g, k8sClient.List(t.Context(), &srcList, &client.ListOptions{
		Namespace: ns.Name,
	}))
	expectToEqual(t, g, len(srcList.Items), 1, "source list") // just `source`
	expectToEqual(t, g, srcList.Items[0].Name, source.Name)

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}

func Test_filterPullRequestsByBaseBranch(t *testing.T) {
	prs := []provider.PullRequest{
		{Number: 1, BaseBranch: "main"},
		{Number: 2, BaseBranch: "develop"},
		{Number: 3, BaseBranch: "main"},
		{Number: 4, BaseBranch: git.DefaultBranch},
	}
	main := []provider.PullRequest{prs[0], prs[2]}
	develop := []provider.PullRequest{prs[1]}
	defaultBranch := []provider.PullRequest{prs[3]}

	tests := []struct {
		name     string
		ref      *sourcev1.GitRepositoryRef
		override string
		want     []provider.PullRequest
	}{
		{"branch", &sourcev1.GitRepositoryRef{Branch: "main"}, "", main},
		{"override beats source", &sourcev1.GitRepositoryRef{Branch: "main"}, "develop", develop},
		{"name refs/heads", &sourcev1.GitRepositoryRef{Name: "refs/heads/develop"}, "", develop},
		{"name refs/pull", &sourcev1.GitRepositoryRef{Name: "refs/pull/1/head"}, "", []provider.PullRequest{}},
		{"tag", &sourcev1.GitRepositoryRef{Tag: "v1"}, "", []provider.PullRequest{}},
		{"semver", &sourcev1.GitRepositoryRef{SemVer: ">=1.0.0"}, "", []provider.PullRequest{}},
		{"commit only", &sourcev1.GitRepositoryRef{Commit: "1111111111111111111111111111111111111111"}, "", []provider.PullRequest{}},
		{"branch and commit", &sourcev1.GitRepositoryRef{Branch: "main", Commit: "1111111111111111111111111111111111111111"}, "", main},
		{"nil ref defaults to git default branch", nil, "", defaultBranch},
		{"empty ref defaults to git default branch", &sourcev1.GitRepositoryRef{}, "", defaultBranch},
		{"override with tag source", &sourcev1.GitRepositoryRef{Tag: "v1"}, "main", main},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			tf := &infrav1.Terraform{}
			if tt.override != "" {
				tf.Spec.BranchPlanner = &infrav1.BranchPlanner{BaseBranch: tt.override}
			}
			source := &sourcev1.GitRepository{Spec: sourcev1.GitRepositorySpec{Reference: tt.ref}}

			server, err := New()
			g.Expect(err).NotTo(gomega.HaveOccurred())

			g.Expect(server.filterPullRequestsByBaseBranch(tf, source, prs)).To(gomega.ConsistOf(tt.want))
		})
	}
}

// This checks that the Secrets of workspace file mappings are copied for each
// pull request with the content of the mapped files in the pull request, that
// a replan is triggered when the content changes after the head commit was
// planned, and that the copies are deleted when the pull request is closed.
func Test_poll_reconcile_file_mappings(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)
	ctx := t.Context()

	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/tf-controller/helloworld",
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, source))

	tfvars := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tfvars",
			Namespace: ns.Name,
		},
		Data: map[string][]byte{
			"default.tfvars": []byte(`name = "main"`),
			"only-in-secret": []byte(`secret = "main"`),
			"unmapped":       []byte(`unmapped = "main"`),
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, tfvars))

	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
		},
		Spec: infrav1.TerraformSpec{
			Path: "./roots/base/",
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
			FileMappings: []infrav1.FileMapping{
				{
					Location:  "workspace",
					Path:      "default.tfvars",
					SecretRef: meta.SecretKeyReference{Name: tfvars.Name, Key: "default.tfvars"},
				},
				{
					Location:  "workspace",
					Path:      "only-in-secret.tfvars",
					SecretRef: meta.SecretKeyReference{Name: tfvars.Name, Key: "only-in-secret"},
				},
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, original))

	prs := []provider.PullRequest{
		{
			Repository: provider.Repository{Org: "fake-org", Name: "fake-name"},
			Number:     1,
			BaseBranch: "main",
			HeadBranch: "test-branch-1",
			HeadSha:    "0123456789abcdef",
			BaseSha:    "fedcba9876543210",
		},
	}

	prContent := `name = "pr"`
	gitProvider := &providerfakes.FakeProvider{}
	gitProvider.GetFileContentStub = func(_ context.Context, _ provider.Repository, path, ref string) ([]byte, error) {
		if ref == prs[0].HeadSha && path == "roots/base/default.tfvars" {
			return []byte(prContent), nil
		}
		return nil, provider.ErrFileNotFound
	}

	server, err := New(WithClusterClient(k8sClient))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, gitProvider))

	secretName := config.FileMappingSecretName(original.Name, tfvars.Name, "1")
	secret := &corev1.Secret{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: secretName}, secret))
	expectToEqual(t, g, string(secret.Data["default.tfvars"]), `name = "pr"`)
	expectToEqual(t, g, string(secret.Data["only-in-secret"]), `secret = "main"`)
	expectToEqual(t, g, string(secret.Data["unmapped"]), `unmapped = "main"`)
	expectToEqual(t, g, secret.Labels[bpconfig.LabelPrimaryResourceKey], original.Name)
	expectToEqual(t, g, secret.Labels[bpconfig.LabelPRIDKey], "1")

	branchTF := &infrav1.Terraform{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: config.PullRequestObjectName(original.Name, "1")}, branchTF))
	expectToEqual(t, g, branchTF.Spec.FileMappings[0].SecretRef.Name, secretName)
	expectToEqual(t, g, branchTF.Spec.FileMappings[1].SecretRef.Name, secretName)

	// Nothing changed: the files are not fetched again, and there is no replan.
	branchTF.Status.LastPlannedRevision = "test-branch-1@sha1:" + prs[0].HeadSha
	expectToSucceed(t, g, k8sClient.Status().Update(ctx, branchTF))
	calls := gitProvider.GetFileContentCallCount()

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, gitProvider))

	expectToEqual(t, g, gitProvider.GetFileContentCallCount(), calls)
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKeyFromObject(branchTF), branchTF))
	g.Expect(branchTF.Annotations).NotTo(gomega.HaveKey(meta.ReconcileRequestAnnotation))

	// A new commit is planned before the Secret is updated.
	prs[0].HeadSha = "1123456789abcdef"
	prContent = `name = "pr-updated"`
	branchTF.Status.LastPlannedRevision = "test-branch-1@sha1:" + prs[0].HeadSha
	expectToSucceed(t, g, k8sClient.Status().Update(ctx, branchTF))

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, gitProvider))

	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	expectToEqual(t, g, string(secret.Data["default.tfvars"]), `name = "pr-updated"`)

	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKeyFromObject(branchTF), branchTF))
	expectToEqual(t, g, branchTF.Status.LastPlannedRevision, "")
	g.Expect(branchTF.Annotations).To(gomega.HaveKey(meta.ReconcileRequestAnnotation))
	g.Expect(branchTF.Annotations).NotTo(gomega.HaveKey(bpconfig.AnnotationCommentIDKey))

	// Closing the pull request deletes the copy, but not the original.
	expectToSucceed(t, g, server.reconcile(ctx, original, source, nil, gitProvider))

	var secretList corev1.SecretList
	expectToSucceed(t, g, k8sClient.List(ctx, &secretList, client.InNamespace(ns.Name)))
	expectToEqual(t, g, len(secretList.Items), 1, "secret list")
	expectToEqual(t, g, secretList.Items[0].Name, tfvars.Name)

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}

// This checks that the files of a Secret annotated with a source directory are
// looked up in that directory, and that the files deleted by the pull request
// are dropped from the file mappings and the tfvars files.
func Test_poll_reconcile_file_mappings_source_dir(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)
	ctx := t.Context()

	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/tf-controller/helloworld",
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, source))

	tfvars := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tfvars",
			Namespace: ns.Name,
			Annotations: map[string]string{
				bpconfig.AnnotationFileMappingSourceDir: "charts/vpc/tfvars",
			},
		},
		Data: map[string][]byte{
			"default.tfvars": []byte(`name = "main"`),
			"mtp.tfvars":     []byte(`mtp = "main"`),
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, tfvars))

	mapping := func(key string) infrav1.FileMapping {
		return infrav1.FileMapping{
			Location:  "workspace",
			Path:      key,
			SecretRef: meta.SecretKeyReference{Name: tfvars.Name, Key: key},
		}
	}

	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
		},
		Spec: infrav1.TerraformSpec{
			Path: "./roots/base/",
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
			FileMappings: []infrav1.FileMapping{mapping("default.tfvars"), mapping("mtp.tfvars")},
			TfVarsFiles:  []string{"roots/base/default.tfvars", "roots/base/mtp.tfvars"},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, original))

	prs := []provider.PullRequest{
		{
			Repository: provider.Repository{Org: "fake-org", Name: "fake-name"},
			Number:     1,
			BaseBranch: "main",
			HeadBranch: "test-branch-1",
			HeadSha:    "0123456789abcdef",
			BaseSha:    "fedcba9876543210",
		},
	}

	// default.tfvars is changed and mtp.tfvars is deleted by the pull request.
	files := map[string]map[string]string{
		prs[0].HeadSha: {"charts/vpc/tfvars/default.tfvars": `name = "pr"`},
		prs[0].BaseSha: {"charts/vpc/tfvars/default.tfvars": `name = "main"`, "charts/vpc/tfvars/mtp.tfvars": `mtp = "main"`},
	}
	gitProvider := &providerfakes.FakeProvider{}
	gitProvider.GetFileContentStub = func(_ context.Context, _ provider.Repository, path, ref string) ([]byte, error) {
		if content, ok := files[ref][path]; ok {
			return []byte(content), nil
		}
		return nil, provider.ErrFileNotFound
	}

	server, err := New(WithClusterClient(k8sClient))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, gitProvider))

	secretName := config.FileMappingSecretName(original.Name, tfvars.Name, "1")
	secret := &corev1.Secret{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: secretName}, secret))
	expectToEqual(t, g, string(secret.Data["default.tfvars"]), `name = "pr"`)
	g.Expect(secret.Data).NotTo(gomega.HaveKey("mtp.tfvars"))

	branchTF := &infrav1.Terraform{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: config.PullRequestObjectName(original.Name, "1")}, branchTF))
	expectToEqual(t, g, len(branchTF.Spec.FileMappings), 1)
	expectToEqual(t, g, branchTF.Spec.FileMappings[0].SecretRef, meta.SecretKeyReference{Name: secretName, Key: "default.tfvars"})
	expectToEqual(t, g, branchTF.Spec.TfVarsFiles, []string{"roots/base/default.tfvars"})

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}

// This checks that the candidate files of a Secret are mapped and loaded as
// tfvars files when they exist in the pull request, in the candidates order,
// so files added and deleted by the pull request are taken into account.
func Test_poll_reconcile_file_mappings_source_files(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)
	ctx := t.Context()

	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/tf-controller/helloworld",
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, source))

	tfvars := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tfvars",
			Namespace: ns.Name,
			Annotations: map[string]string{
				bpconfig.AnnotationFileMappingSourceDir:   "charts/vpc/tfvars",
				bpconfig.AnnotationFileMappingSourceFiles: `["default.tfvars","mtp.tfvars","mtp.us-east-1.tfvars"]`,
			},
		},
		Data: map[string][]byte{
			"default.tfvars": []byte(`name = "main"`),
			"mtp.tfvars":     []byte(`mtp = "main"`),
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, tfvars))

	mapping := func(secretName, key string) infrav1.FileMapping {
		return infrav1.FileMapping{
			Location:  "workspace",
			Path:      key,
			SecretRef: meta.SecretKeyReference{Name: secretName, Key: key},
		}
	}

	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
		},
		Spec: infrav1.TerraformSpec{
			Path: "./roots/base/",
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
			FileMappings: []infrav1.FileMapping{
				mapping(tfvars.Name, "default.tfvars"),
				mapping(tfvars.Name, "mtp.tfvars"),
				{Location: "home", Path: ".tofurc", SecretRef: meta.SecretKeyReference{Name: "tofurc", Key: "tofurc"}},
			},
			TfVarsFiles: []string{"roots/base/default.tfvars", "roots/base/mtp.tfvars", "extra.tfvars"},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, original))

	prs := []provider.PullRequest{
		{
			Repository: provider.Repository{Org: "fake-org", Name: "fake-name"},
			Number:     1,
			BaseBranch: "main",
			HeadBranch: "test-branch-1",
			HeadSha:    "0123456789abcdef",
			BaseSha:    "fedcba9876543210",
		},
	}

	// The pull request changes default.tfvars, deletes mtp.tfvars and adds
	// mtp.us-east-1.tfvars.
	gitProvider := &providerfakes.FakeProvider{}
	gitProvider.GetFileContentStub = func(_ context.Context, _ provider.Repository, path, ref string) ([]byte, error) {
		expectToEqual(t, g, ref, prs[0].HeadSha)
		switch path {
		case "charts/vpc/tfvars/default.tfvars":
			return []byte(`name = "pr"`), nil
		case "charts/vpc/tfvars/mtp.us-east-1.tfvars":
			return []byte(`region = "us-east-1"`), nil
		}
		return nil, provider.ErrFileNotFound
	}

	server, err := New(WithClusterClient(k8sClient))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, gitProvider))

	secretName := config.FileMappingSecretName(original.Name, tfvars.Name, "1")
	secret := &corev1.Secret{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: secretName}, secret))
	expectToEqual(t, g, len(secret.Data), 2)
	expectToEqual(t, g, string(secret.Data["default.tfvars"]), `name = "pr"`)
	expectToEqual(t, g, string(secret.Data["mtp.us-east-1.tfvars"]), `region = "us-east-1"`)

	branchTF := &infrav1.Terraform{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: config.PullRequestObjectName(original.Name, "1")}, branchTF))
	expectToEqual(t, g, branchTF.Spec.FileMappings, []infrav1.FileMapping{
		mapping(secretName, "default.tfvars"),
		mapping(secretName, "mtp.us-east-1.tfvars"),
		original.Spec.FileMappings[2],
	})
	expectToEqual(t, g, branchTF.Spec.TfVarsFiles, []string{"roots/base/default.tfvars", "roots/base/mtp.us-east-1.tfvars", "extra.tfvars"})

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}

// This checks that reconciling an unchanged pull request does not update its
// Terraform object, which would trigger a replan on every poll.
func Test_poll_reconcile_unchanged(t *testing.T) {
	g := gomega.NewWithT(t)
	ns := newNamespace(t, g)
	ctx := t.Context()

	source := &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original-source",
			Namespace: ns.Name,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/tf-controller/helloworld",
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "main",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, source))

	original := &infrav1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "original",
			Namespace: ns.Name,
		},
		Spec: infrav1.TerraformSpec{
			SourceRef: infrav1.CrossNamespaceSourceReference{
				Name: source.Name,
				Kind: "GitRepository",
			},
		},
	}
	expectToSucceed(t, g, k8sClient.Create(ctx, original))
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKeyFromObject(original), original))

	prs := []provider.PullRequest{
		{
			Repository: provider.Repository{Org: "fake-org", Name: "fake-name"},
			Number:     1,
			BaseBranch: "main",
			HeadBranch: "test-branch-1",
			HeadSha:    "0123456789abcdef",
		},
	}

	server, err := New(WithClusterClient(k8sClient))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, &providerfakes.FakeProvider{}))

	branchTF := &infrav1.Terraform{}
	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: config.PullRequestObjectName(original.Name, "1")}, branchTF))
	branchTF.Status.LastPlannedRevision = "test-branch-1@sha1:" + prs[0].HeadSha
	expectToSucceed(t, g, k8sClient.Status().Update(ctx, branchTF))
	generation := branchTF.Generation

	expectToSucceed(t, g, server.reconcile(ctx, original, source, prs, &providerfakes.FakeProvider{}))

	expectToSucceed(t, g, k8sClient.Get(ctx, client.ObjectKeyFromObject(branchTF), branchTF))
	expectToEqual(t, g, branchTF.Generation, generation)
	g.Expect(branchTF.Annotations).NotTo(gomega.HaveKey(meta.ReconcileRequestAnnotation))

	t.Cleanup(func() { expectToSucceed(t, g, k8sClient.Delete(context.Background(), ns)) })
}
