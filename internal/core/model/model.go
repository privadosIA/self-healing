// Package model holds the domain types shared by the core and the ports.
package model

import "time"

// WebhookEvent is a CI failure notification normalized from a Gitea webhook.
type WebhookEvent struct {
	RepoID         int64  `json:"repo_id"`
	RepoFullName   string `json:"repo_full_name"`
	CloneURL       string `json:"clone_url"`
	DefaultBranch  string `json:"default_branch"`
	CommitSHA      string `json:"commit_sha"`
	Branch         string `json:"branch"`
	PipelineStatus string `json:"pipeline_status"`
	JobName        string `json:"job_name"`
	RunID          int64  `json:"run_id"`
	DeliveryID     string `json:"delivery_id"`
}

// JobStatus is the lifecycle state of a fix job.
type JobStatus string

const (
	JobQueued   JobStatus = "queued"
	JobRunning  JobStatus = "running"
	JobSkipped  JobStatus = "skipped"
	JobFailed   JobStatus = "failed"
	JobPROpened JobStatus = "pr_opened"
)

// Terminal reports whether no further transition is possible.
func (s JobStatus) Terminal() bool {
	return s == JobSkipped || s == JobFailed || s == JobPROpened
}

// Job is a persisted fix job.
type Job struct {
	ID        int64
	Event     WebhookEvent
	RuleID    string
	Status    JobStatus
	PRURL     string
	TokensIn  int
	TokensOut int
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// JobResult is what a finished job records. Status must be terminal.
type JobResult struct {
	Status    JobStatus
	RuleID    string
	PRURL     string
	TokensIn  int
	TokensOut int
	Error     string
}
