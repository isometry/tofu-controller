package provider_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flux-iac/tofu-controller/internal/git/provider"
	"github.com/flux-iac/tofu-controller/internal/git/provider/providerfakes"
)

func TestUpsertComment(t *testing.T) {
	marker := "<!-- marker -->"
	pr := provider.PullRequest{Number: 1}

	t.Run("creates the comment when there is none with the marker", func(t *testing.T) {
		p := &providerfakes.FakeProvider{}
		p.GetLastCommentsReturns([]*provider.Comment{
			{ID: 1, Body: "> " + marker + "\nquoted by someone"},
		}, nil)
		p.AddCommentToPullRequestReturns(&provider.Comment{ID: 2}, nil)

		comment, err := provider.UpsertComment(context.Background(), p, pr, marker, []byte("plan"))
		require.NoError(t, err)

		assert.Equal(t, 2, comment.ID)
		assert.Equal(t, 0, p.UpdateCommentOfPullRequestCallCount())
		_, _, since := p.GetLastCommentsArgsForCall(0)
		assert.True(t, since.IsZero(), "all the comments are searched")
		_, _, body := p.AddCommentToPullRequestArgsForCall(0)
		assert.Equal(t, marker+"\nplan", string(body))
	})

	t.Run("updates the most recent comment with the marker", func(t *testing.T) {
		p := &providerfakes.FakeProvider{}
		p.GetLastCommentsReturns([]*provider.Comment{
			{ID: 3, Body: "!replan"},
			{ID: 2, Body: marker + "\nnewer plan"},
			{ID: 1, Body: marker + "\nolder plan"},
		}, nil)

		comment, err := provider.UpsertComment(context.Background(), p, pr, marker, []byte("plan"))
		require.NoError(t, err)

		assert.Equal(t, 2, comment.ID)
		assert.Equal(t, 0, p.AddCommentToPullRequestCallCount())
		_, _, id, body := p.UpdateCommentOfPullRequestArgsForCall(0)
		assert.Equal(t, 2, id)
		assert.Equal(t, marker+"\nplan", string(body))
	})
}
