// Package webhook validates GitHub workflow_job deliveries and queues them.
package webhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/github"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/labels"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/runner"
)

// MaxBody caps accepted payloads (GitHub caps webhooks at 25 MB; workflow_job is small).
const MaxBody = 1 << 20

// Handler turns a Function URL request into a queue message.
type Handler struct {
	Secret         func(ctx context.Context) ([]byte, error)
	Queue          runner.Queue
	Repository     string // owner/repo; deliveries for other repos are rejected
	RunnerLabels   []string
	RequiredLabels []string
	Log            *slog.Logger
}

func resp(code int, msg string) events.LambdaFunctionURLResponse {
	return events.LambdaFunctionURLResponse{StatusCode: code, Body: msg, Headers: map[string]string{"Content-Type": "text/plain"}}
}

func header(h map[string]string, k string) string {
	for hk, v := range h {
		if strings.EqualFold(hk, k) {
			return v
		}
	}
	return ""
}

// Handle processes one delivery.
func (h *Handler) Handle(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	if req.RequestContext.HTTP.Method != http.MethodPost {
		return resp(http.StatusMethodNotAllowed, "method not allowed"), nil
	}
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return resp(http.StatusBadRequest, "bad body"), nil
		}
		body = b
	}
	if len(body) > MaxBody {
		return resp(http.StatusRequestEntityTooLarge, "too large"), nil
	}
	delivery := header(req.Headers, "X-GitHub-Delivery")
	log := h.Log.With("delivery", delivery, "source_ip", req.RequestContext.HTTP.SourceIP)

	secret, err := h.Secret(ctx)
	if err != nil {
		log.Error("load webhook secret", "err", err)
		return resp(http.StatusInternalServerError, "unavailable"), err
	}
	if !github.VerifySignature(secret, body, header(req.Headers, "X-Hub-Signature-256")) {
		log.Warn("audit: webhook signature rejected", "audit", true, "event", "webhook.rejected")
		return resp(http.StatusUnauthorized, "bad signature"), nil
	}

	switch header(req.Headers, "X-GitHub-Event") {
	case "ping":
		return resp(http.StatusOK, "pong"), nil
	case "workflow_job":
	default:
		return resp(http.StatusAccepted, "ignored"), nil
	}

	var ev github.WorkflowJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return resp(http.StatusBadRequest, "bad json"), nil
	}
	if !strings.EqualFold(ev.Repository.FullName, h.Repository) {
		log.Warn("audit: webhook for foreign repository", "audit", true, "event", "webhook.rejected", "repository", ev.Repository.FullName)
		return resp(http.StatusForbidden, "wrong repository"), nil
	}

	j := ev.WorkflowJob
	m := runner.Message{Source: "webhook", Delivery: delivery, JobID: j.ID, RunID: j.RunID, Labels: j.Labels,
		RunnerName: j.RunnerName, Conclusion: j.Conclusion, Actor: ev.Sender.Login,
		Workflow: j.WorkflowName, HeadSHA: j.HeadSHA, HeadBranch: j.HeadBranch}
	switch ev.Action {
	case "queued":
		if !labels.Match(j.Labels, h.RunnerLabels, h.RequiredLabels) {
			return resp(http.StatusAccepted, "labels not served"), nil
		}
		m.Type = runner.TypeQueued
	case "completed":
		if _, ok := runner.ParseRunnerName(j.RunnerName); !ok {
			return resp(http.StatusAccepted, "not our runner"), nil
		}
		m.Type = runner.TypeCompleted
	default:
		return resp(http.StatusAccepted, "ignored"), nil
	}
	if err := h.Queue.Send(ctx, m); err != nil {
		log.Error("enqueue", "err", err)
		// 5xx lets GitHub's redelivery and our sweeper recover.
		return resp(http.StatusServiceUnavailable, "retry"), nil
	}
	log.Info("audit: webhook accepted", "audit", true, "event", "webhook.accepted", "action", ev.Action,
		"job_id", j.ID, "run_id", j.RunID, "actor", ev.Sender.Login, "workflow", j.WorkflowName, "head_sha", j.HeadSHA)
	return resp(http.StatusAccepted, "queued"), nil
}
