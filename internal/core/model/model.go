// Package model holds the domain types shared by the core and the ports.
package model

// WebhookEvent is a CI failure notification normalized from a Gitea webhook.
type WebhookEvent struct {
	RepoID         int64
	RepoFullName   string
	CloneURL       string
	DefaultBranch  string
	CommitSHA      string
	Branch         string
	PipelineStatus string
	JobName        string
	RunID          int64
	DeliveryID     string
}
