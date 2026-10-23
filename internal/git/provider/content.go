package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jenkins-x/go-scm/scm"
)

// getFileContent fetches a file through the go-scm content service. Not every
// driver maps a 404 to scm.ErrNotFound (e.g. GitLab), so the response status
// is checked as well.
func getFileContent(ctx context.Context, client *scm.Client, repo Repository, path, ref string) ([]byte, error) {
	content, res, err := client.Contents.Find(ctx, repo.String(), path, ref)
	if errors.Is(err, scm.ErrNotFound) || (res != nil && res.Status == http.StatusNotFound) {
		return nil, fmt.Errorf("%s@%s: %w", path, ref, ErrFileNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to get file content %s@%s: %w", path, ref, err)
	}

	return content.Data, nil
}

func (p *GitHubProvider) GetFileContent(ctx context.Context, repo Repository, path, ref string) ([]byte, error) {
	return getFileContent(ctx, p.client, repo, path, ref)
}

func (p *GitLabProvider) GetFileContent(ctx context.Context, repo Repository, path, ref string) ([]byte, error) {
	return getFileContent(ctx, p.client, repo, path, ref)
}

func (p *scmProvider) GetFileContent(ctx context.Context, repo Repository, path, ref string) ([]byte, error) {
	return getFileContent(ctx, p.client, repo, path, ref)
}
