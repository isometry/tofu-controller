package config

import "fmt"

// CommentMarker returns the hidden marker at the start of the pull request
// comment of a Terraform object, used to find the comment to update.
func CommentMarker(namespace, name, clusterName string) string {
	return fmt.Sprintf("<!-- tofu-controller branch-planner: %s/%s@%s -->", namespace, name, clusterName)
}

// CommentSubject returns how a Terraform object is referred to in pull request
// comments, so the comments of several objects and clusters can be told apart.
func CommentSubject(namespace, name, clusterName string) string {
	subject := fmt.Sprintf("`%s/%s`", namespace, name)
	if clusterName != "" {
		subject += fmt.Sprintf(" on `%s`", clusterName)
	}

	return subject
}
