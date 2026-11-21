// Package runner holds the dispatch and sweep logic. AWS and GitHub are
// behind small interfaces so the logic is unit tested without network.
package runner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/github"
)

// Message types carried on the queue.
const (
	TypeQueued    = "queued"
	TypeCompleted = "completed"
)

// Message is the queue payload produced by the webhook and the sweeper.
type Message struct {
	Type       string   `json:"type"`
	Source     string   `json:"source"` // webhook | sweeper
	Delivery   string   `json:"delivery,omitempty"`
	JobID      int64    `json:"job_id"`
	RunID      int64    `json:"run_id,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	RunnerName string   `json:"runner_name,omitempty"`
	Conclusion string   `json:"conclusion,omitempty"`
	Actor      string   `json:"actor,omitempty"`
	Workflow   string   `json:"workflow,omitempty"`
	HeadSHA    string   `json:"head_sha,omitempty"`
	HeadBranch string   `json:"head_branch,omitempty"`
}

// VM statuses.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusLost    = "lost"
	StatusReaped  = "reaped"
	StatusIdle    = "idle"
	StatusFailed  = "failed"
)

// VM is one MicroVM runner tracked in the state table.
type VM struct {
	RunnerName string
	RunnerID   int64
	MicrovmID  string
	JobID      int64 // job it was launched for; GitHub may hand it another one
	Status     string
	StartedAt  time.Time
}

// Store is the DynamoDB state table.
type Store interface {
	// Claim reserves a job for dispatch. It succeeds when the job was never
	// claimed, was released, or was claimed before staleBefore (zero = never).
	Claim(ctx context.Context, jobID int64, now, staleBefore time.Time) (attempt int, ok bool, err error)
	ReleaseClaim(ctx context.Context, jobID int64) error
	PutVM(ctx context.Context, vm VM) error
	GetVM(ctx context.Context, runnerName string) (*VM, error)
	EndVM(ctx context.Context, runnerName, status string, now time.Time) error
	ActiveVMs(ctx context.Context) ([]VM, error)
}

// RunInput is what the dispatcher asks the MicroVM service for.
type RunInput struct {
	ClientToken string
	Payload     string
}

// VM states returned by MicroVMs.State.
const (
	StateGone = "GONE" // terminated or not found
)

// MicroVMs wraps the lambda-microvms API.
type MicroVMs interface {
	Run(ctx context.Context, in RunInput) (string, error)
	State(ctx context.Context, id string) (string, error)
	Terminate(ctx context.Context, id string) error
}

// Params stores the one-time JIT configuration.
type Params interface {
	Put(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
}

// Queue sends messages to the dispatcher.
type Queue interface {
	Send(ctx context.Context, m Message) error
}

// GitHub is the subset of the GitHub App client we need.
type GitHub interface {
	RepoToken(ctx context.Context, owner, repo string) (string, error)
	GenerateJITConfig(ctx context.Context, token, owner, repo, name string, groupID int64, labels []string) (github.JITConfig, error)
	DeleteRunner(ctx context.Context, token, owner, repo string, id int64) error
	RunnerStatus(ctx context.Context, token, owner, repo string, id int64) (exists, busy bool, err error)
	QueuedJobs(ctx context.Context, token, owner, repo string, maxRuns int) ([]github.Job, error)
}

// Config is shared by dispatcher and sweeper.
type Config struct {
	Owner, Repo     string
	Region          string
	RunnerLabels    []string
	RequiredLabels  []string
	RunnerGroupID   int64
	MaxRunners      int
	MaxDuration     time.Duration // MicroVM hard lifetime
	ParamPrefix     string        // e.g. /gha/my-org/my-repo
	IdleTimeout     time.Duration // runner registered but never got a job
	ClaimStaleAfter time.Duration // sweeper may re-dispatch a job claimed earlier than this
	QueuedGrace     time.Duration // sweeper waits this long before re-dispatching a queued job
}

// ErrThrottled means the runner cap is reached; the message is retried.
var ErrThrottled = errors.New("runner capacity reached")

// RunnerName builds the deterministic runner name for a job attempt.
func RunnerName(jobID int64, attempt int) string {
	return fmt.Sprintf("mvm-%d-%d", jobID, attempt)
}

// ParseRunnerName returns the job id of a runner name we created.
func ParseRunnerName(name string) (int64, bool) {
	parts := strings.Split(name, "-")
	if len(parts) != 3 || parts[0] != "mvm" {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	if _, err := strconv.Atoi(parts[2]); err != nil {
		return 0, false
	}
	return id, true
}

// ParamName is the SSM parameter holding the JIT config of a runner.
func (c Config) ParamName(runnerName string) string {
	return strings.TrimRight(c.ParamPrefix, "/") + "/jit/" + runnerName
}
