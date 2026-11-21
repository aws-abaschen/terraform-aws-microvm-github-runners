package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/github"
)

type claim struct {
	attempt int
	status  string
	at      time.Time
}

type fakeStore struct {
	mu     sync.Mutex
	claims map[int64]*claim
	vms    map[string]*VM
}

func newStore() *fakeStore {
	return &fakeStore{claims: map[int64]*claim{}, vms: map[string]*VM{}}
}

func (f *fakeStore) Claim(_ context.Context, id int64, now, stale time.Time) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.claims[id]
	if c != nil && c.status != "released" && !(c.at.Before(stale)) {
		return 0, false, nil
	}
	if c == nil {
		c = &claim{}
		f.claims[id] = c
	}
	c.attempt++
	c.status, c.at = "claimed", now
	return c.attempt, true, nil
}

func (f *fakeStore) ReleaseClaim(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.claims[id]; c != nil {
		c.status = "released"
	}
	return nil
}

func (f *fakeStore) PutVM(_ context.Context, vm VM) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := vm
	f.vms[vm.RunnerName] = &v
	return nil
}

func (f *fakeStore) GetVM(_ context.Context, name string) (*VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.vms[name]; ok {
		c := *v
		return &c, nil
	}
	return nil, nil
}

func (f *fakeStore) EndVM(_ context.Context, name, status string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.vms[name]; ok {
		v.Status = status
	}
	return nil
}

func (f *fakeStore) ActiveVMs(context.Context) ([]VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []VM
	for _, v := range f.vms {
		if v.Status == StatusRunning {
			out = append(out, *v)
		}
	}
	return out, nil
}

type fakeVMs struct {
	runs       []RunInput
	terminated []string
	states     map[string]string
	runErr     error
}

func (f *fakeVMs) Run(_ context.Context, in RunInput) (string, error) {
	if f.runErr != nil {
		return "", f.runErr
	}
	f.runs = append(f.runs, in)
	return fmt.Sprintf("mvm-id-%d", len(f.runs)), nil
}

func (f *fakeVMs) State(_ context.Context, id string) (string, error) {
	if s, ok := f.states[id]; ok {
		return s, nil
	}
	return "RUNNING", nil
}

func (f *fakeVMs) Terminate(_ context.Context, id string) error {
	f.terminated = append(f.terminated, id)
	return nil
}

type fakeParams struct{ values map[string]string }

func (f *fakeParams) Put(_ context.Context, n, v string) error { f.values[n] = v; return nil }
func (f *fakeParams) Delete(_ context.Context, n string) error { delete(f.values, n); return nil }

type runnerState struct{ exists, busy bool }

type fakeGitHub struct {
	nextID  int64
	deleted []int64
	runners map[int64]runnerState
	queued  []github.Job
}

func (f *fakeGitHub) RepoToken(context.Context, string, string) (string, error) { return "tok", nil }
func (f *fakeGitHub) GenerateJITConfig(_ context.Context, _, _, _, _ string, _ int64, _ []string) (github.JITConfig, error) {
	f.nextID++
	return github.JITConfig{Encoded: "jit-secret", RunnerID: f.nextID}, nil
}
func (f *fakeGitHub) DeleteRunner(_ context.Context, _, _, _ string, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeGitHub) RunnerStatus(_ context.Context, _, _, _ string, id int64) (bool, bool, error) {
	s, ok := f.runners[id]
	if !ok {
		return true, true, nil
	}
	return s.exists, s.busy, nil
}
func (f *fakeGitHub) QueuedJobs(context.Context, string, string, string, int) ([]github.Job, error) {
	return f.queued, nil
}

type fakeQueue struct{ sent []Message }

func (f *fakeQueue) Send(_ context.Context, m Message) error { f.sent = append(f.sent, m); return nil }

var errBoom = errors.New("boom")

func testCfg() Config {
	return Config{
		Owner: "acme", Repo: "app", Region: "eu-west-1",
		RunnerLabels:   []string{"self-hosted", "linux", "arm64", "microvm"},
		RequiredLabels: []string{"microvm"},
		RunnerGroupID:  1, MaxRunners: 2, MaxDuration: 65 * time.Minute,
		ParamPrefix: "/gha/acme/app", IdleTimeout: 10 * time.Minute,
		ClaimStaleAfter: 5 * time.Minute, QueuedGrace: 2 * time.Minute,
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
