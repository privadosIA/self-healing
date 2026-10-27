package listener

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

// Headers sent by Gitea with every webhook.
const (
	eventHeader    = "X-Gitea-Event"
	deliveryHeader = "X-Gitea-Delivery"
)

// autoFixPrefix marks branches created by healerd; events on them are ignored
// so a failing fix cannot trigger another fix.
const autoFixPrefix = "auto-fix/"

// errIgnored means the event is valid but needs no job.
type errIgnored string

func (e errIgnored) Error() string { return string(e) }

// NewWebhookHandler returns the handler for Gitea webhooks. It must be wrapped
// by RequireSignature: it does no authentication itself.
//
// Responses: 202 job queued, 200 duplicate delivery, 204 event ignored,
// 400 malformed payload, 405 wrong method, 500 store failure. m may be nil.
func NewWebhookHandler(store ports.JobEnqueuer, log *slog.Logger, m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		delivery := r.Header.Get(deliveryHeader)
		if delivery == "" {
			m.webhook(resultMalformed)
			http.Error(w, "missing "+deliveryHeader, http.StatusBadRequest)
			return
		}

		ev, err := parseEvent(r.Header.Get(eventHeader), r)
		var ignored errIgnored
		switch {
		case errors.As(err, &ignored):
			m.webhook(resultIgnored)
			log.Info("event ignored", "delivery_id", delivery, "reason", string(ignored))
			w.WriteHeader(http.StatusNoContent)
			return
		case err != nil:
			m.webhook(resultMalformed)
			log.Warn("malformed webhook", "delivery_id", delivery, "reason", err.Error())
			http.Error(w, "malformed payload", http.StatusBadRequest)
			return
		}
		ev.DeliveryID = delivery

		created, err := store.Enqueue(r.Context(), ev)
		if err != nil {
			m.webhook(resultError)
			log.Error("cannot enqueue job", "delivery_id", delivery, "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		if !created {
			m.webhook(resultDuplicate)
			log.Info("duplicate delivery", "delivery_id", delivery)
			w.WriteHeader(http.StatusOK)
			return
		}
		m.webhook(resultQueued)
		log.Info("job queued", "delivery_id", delivery, "repo", ev.RepoFullName, "branch", ev.Branch)
		w.WriteHeader(http.StatusAccepted)
	})
}

type repositoryPayload struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}

type workflowJobPayload struct {
	WorkflowJob struct {
		RunID      int64  `json:"run_id"`
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Conclusion string `json:"conclusion"`
	} `json:"workflow_job"`
	Repository repositoryPayload `json:"repository"`
}

type statusPayload struct {
	State    string `json:"state"`
	SHA      string `json:"sha"`
	Context  string `json:"context"`
	Branches []struct {
		Name string `json:"name"`
	} `json:"branches"`
	Repository repositoryPayload `json:"repository"`
}

// parseEvent turns a webhook into a WebhookEvent. It returns errIgnored for
// events that are valid but irrelevant, and a plain error for malformed ones.
func parseEvent(event string, r *http.Request) (model.WebhookEvent, error) {
	var ev model.WebhookEvent
	switch event {
	case "workflow_job":
		var p workflowJobPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return ev, fmt.Errorf("invalid JSON: %w", err)
		}
		if p.WorkflowJob.Conclusion != "failure" {
			return ev, errIgnored("workflow_job is not a failure")
		}
		ev = model.WebhookEvent{
			CommitSHA:      p.WorkflowJob.HeadSHA,
			Branch:         trimRef(p.WorkflowJob.HeadBranch),
			PipelineStatus: p.WorkflowJob.Conclusion,
			JobName:        p.WorkflowJob.Name,
			RunID:          p.WorkflowJob.RunID,
		}
		applyRepository(&ev, p.Repository)
	case "status":
		var p statusPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return ev, fmt.Errorf("invalid JSON: %w", err)
		}
		if p.State != "failure" && p.State != "error" {
			return ev, errIgnored("status is not a failure")
		}
		ev = model.WebhookEvent{
			CommitSHA:      p.SHA,
			PipelineStatus: p.State,
			JobName:        p.Context,
		}
		if len(p.Branches) > 0 {
			ev.Branch = trimRef(p.Branches[0].Name)
		}
		applyRepository(&ev, p.Repository)
	default:
		return ev, errIgnored("unsupported event type")
	}

	if err := validate(ev); err != nil {
		return ev, err
	}
	if strings.HasPrefix(ev.Branch, autoFixPrefix) {
		return ev, errIgnored("event on an auto-fix branch")
	}
	return ev, nil
}

func applyRepository(ev *model.WebhookEvent, repo repositoryPayload) {
	ev.RepoID = repo.ID
	ev.RepoFullName = repo.FullName
	ev.CloneURL = repo.CloneURL
	ev.DefaultBranch = repo.DefaultBranch
}

func trimRef(branch string) string {
	return strings.TrimPrefix(branch, "refs/heads/")
}

func validate(ev model.WebhookEvent) error {
	var missing []string
	if ev.RepoID == 0 {
		missing = append(missing, "repository.id")
	}
	if ev.RepoFullName == "" {
		missing = append(missing, "repository.full_name")
	}
	if ev.CloneURL == "" {
		missing = append(missing, "repository.clone_url")
	}
	if ev.CommitSHA == "" {
		missing = append(missing, "commit sha")
	}
	if ev.Branch == "" {
		missing = append(missing, "branch")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing fields: %s", strings.Join(missing, ", "))
	}
	return nil
}
