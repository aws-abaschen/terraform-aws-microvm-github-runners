package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/runner"
)

type q struct{ sent []runner.Message }

func (f *q) Send(_ context.Context, m runner.Message) error { f.sent = append(f.sent, m); return nil }

func req(body, event, secret string) events.LambdaFunctionURLRequest {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	r := events.LambdaFunctionURLRequest{
		Body: body,
		Headers: map[string]string{
			"x-hub-signature-256": "sha256=" + hex.EncodeToString(m.Sum(nil)),
			"x-github-event":      event,
			"x-github-delivery":   "d-1",
		},
	}
	r.RequestContext.HTTP.Method = "POST"
	return r
}

func handler(queue *q) *Handler {
	return &Handler{
		Secret:         func(context.Context) ([]byte, error) { return []byte("s"), nil },
		Queue:          queue,
		Repository:     "acme/app",
		RunnerLabels:   []string{"self-hosted", "linux", "arm64", "microvm"},
		RequiredLabels: []string{"microvm"},
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

const queuedBody = `{"action":"queued","workflow_job":{"id":1,"run_id":2,"labels":["self-hosted","microvm"]},"repository":{"full_name":"acme/app"},"sender":{"login":"dev"}}`

func TestQueuedIsEnqueued(t *testing.T) {
	queue := &q{}
	res, _ := handler(queue).Handle(context.Background(), req(queuedBody, "workflow_job", "s"))
	if res.StatusCode != 202 || len(queue.sent) != 1 || queue.sent[0].Type != runner.TypeQueued || queue.sent[0].Actor != "dev" {
		t.Fatalf("status %d sent %+v", res.StatusCode, queue.sent)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		r    events.LambdaFunctionURLRequest
		code int
	}{
		{"bad signature", req(queuedBody, "workflow_job", "wrong"), 401},
		{"foreign repo", req(`{"action":"queued","workflow_job":{"labels":["microvm"]},"repository":{"full_name":"evil/app"}}`, "workflow_job", "s"), 403},
		{"other labels", req(`{"action":"queued","workflow_job":{"labels":["ubuntu-latest"]},"repository":{"full_name":"acme/app"}}`, "workflow_job", "s"), 202},
		{"other event", req(`{}`, "push", "s"), 202},
		{"ping", req(`{}`, "ping", "s"), 200},
		{"completed hosted runner", req(`{"action":"completed","workflow_job":{"runner_name":"GitHub Actions 2"},"repository":{"full_name":"acme/app"}}`, "workflow_job", "s"), 202},
	}
	for _, c := range cases {
		queue := &q{}
		res, _ := handler(queue).Handle(context.Background(), c.r)
		if res.StatusCode != c.code || len(queue.sent) != 0 {
			t.Errorf("%s: status %d sent %d", c.name, res.StatusCode, len(queue.sent))
		}
	}
	get := req(queuedBody, "workflow_job", "s")
	get.RequestContext.HTTP.Method = "GET"
	if res, _ := handler(&q{}).Handle(context.Background(), get); res.StatusCode != 405 {
		t.Errorf("GET: %d", res.StatusCode)
	}
}

func TestCompletedOurRunner(t *testing.T) {
	queue := &q{}
	body := `{"action":"completed","workflow_job":{"id":1,"runner_name":"mvm-1-1","conclusion":"success"},"repository":{"full_name":"ACME/app"}}`
	res, _ := handler(queue).Handle(context.Background(), req(body, "workflow_job", "s"))
	if res.StatusCode != 202 || len(queue.sent) != 1 || queue.sent[0].Type != runner.TypeCompleted {
		t.Fatalf("status %d sent %+v", res.StatusCode, queue.sent)
	}
}
