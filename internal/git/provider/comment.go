package provider

import (
	"context"
	"strings"
	"time"
)

type Comment struct {
	ID   int
	Link string
	Body string
}

// UpsertComment updates the most recent comment of the pull request starting
// with marker, or creates it if there is none, so there is a single comment per
// marker instead of a new one for each update. The marker is prepended to body.
func UpsertComment(ctx context.Context, p Provider, pr PullRequest, marker string, body []byte) (*Comment, error) {
	body = append([]byte(marker+"\n"), body...)

	comments, err := p.GetLastComments(ctx, pr, time.Time{})
	if err != nil {
		return nil, err
	}

	// Comments are sorted by creation time, most recent first.
	for _, comment := range comments {
		if comment == nil || !strings.HasPrefix(comment.Body, marker) {
			continue
		}

		if err := p.UpdateCommentOfPullRequest(ctx, pr, comment.ID, body); err != nil {
			return nil, err
		}

		return &Comment{ID: comment.ID, Link: comment.Link, Body: string(body)}, nil
	}

	return p.AddCommentToPullRequest(ctx, pr, body)
}
