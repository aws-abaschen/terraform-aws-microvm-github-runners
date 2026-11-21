package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/github"
)

type env struct {
	store  *fakeStore
	vms    *fakeVMs
	params *fakeParams
	gh     *fakeGitHub
	queue  *fakeQueue
	now    time.Time
	d      *Dispatcher
	s      *Sweeper
}

func newEnv() *env {
	e := &env{store: newStore(), vms: &fakeVMs{states: map[string]string{}}, params: &fakeParams{values: map[string]string{}},
		gh: &fakeGitHub{runners: map[int64]runnerState{}}, queue: &fakeQueue{}, now: time.Unix(1_800_000_000, 0)}
	clock := func() time.Time { return e.now }
	e.d = &Dispatcher{Cfg: testCfg(), Store: e.store, VMs: e.vms, Params: e.params, GitHub: e.gh, Log: quiet(), Now: clock}
	e.s = &Sweeper{Cfg: testCfg(), Store: e.store, VMs: e.vms, Params: e.params, GitHub: e.gh, Queue: e.queue, Log: quiet(), Now: clock}
	return e
}

func queued(id int64) Message {
	return Message{Type: TypeQueued, Source: "webhook", JobID: id, Labels: []string{"self-hosted", "microvm"}}
}

func TestQueuedLaunchesOneVM(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	if err := e.d.Handle(ctx, queued(42)); err != nil {
		t.Fatal(err)
	}
	if len(e.vms.runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(e.vms.runs))
	}
	run := e.vms.runs[0]
	if run.ClientToken != "mvm-42-1" {
		t.Fatalf("client token %q", run.ClientToken)
	}
	var p RunPayload
	if err := json.Unmarshal([]byte(run.Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Param != "/gha/acme/app/jit/mvm-42-1" || p.Region != "eu-west-1" {
		t.Fatalf("payload %+v", p)
	}
	if e.params.values[p.Param] != "jit-secret" {
		t.Fatal("jit config not stored")
	}
	if contains(run.Payload, "jit-secret") {
		t.Fatal("secret must never travel in the run payload")
	}
	// Duplicate delivery is ignored.
	if err := e.d.Handle(ctx, queued(42)); err != nil {
		t.Fatal(err)
	}
	if len(e.vms.runs) != 1 {
		t.Fatal("duplicate delivery launched a second VM")
	}
}

func TestIgnoresForeignLabels(t *testing.T) {
	e := newEnv()
	m := queued(1)
	m.Labels = []string{"ubuntu-latest"}
	if err := e.d.Handle(context.Background(), m); err != nil || len(e.vms.runs) != 0 {
		t.Fatalf("err=%v runs=%d", err, len(e.vms.runs))
	}
}

func TestCapacityThrottles(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	for i := int64(1); i <= 2; i++ {
		if err := e.d.Handle(ctx, queued(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.d.Handle(ctx, queued(3)); !errors.Is(err, ErrThrottled) {
		t.Fatalf("want throttled, got %v", err)
	}
	// Claim was released, so the retry can succeed once capacity frees.
	_ = e.d.Handle(ctx, Message{Type: TypeCompleted, RunnerName: "mvm-1-1"})
	if err := e.d.Handle(ctx, queued(3)); err != nil {
		t.Fatalf("retry after capacity freed: %v", err)
	}
}

func TestRunFailureCleansUp(t *testing.T) {
	e := newEnv()
	e.vms.runErr = errBoom
	if err := e.d.Handle(context.Background(), queued(7)); err == nil {
		t.Fatal("want error")
	}
	if len(e.params.values) != 0 {
		t.Fatal("jit param leaked")
	}
	if len(e.gh.deleted) != 1 {
		t.Fatal("runner registration leaked")
	}
	e.vms.runErr = nil
	if err := e.d.Handle(context.Background(), queued(7)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if e.vms.runs[0].ClientToken != "mvm-7-2" {
		t.Fatalf("retry must use a new attempt, got %s", e.vms.runs[0].ClientToken)
	}
}

func TestCompletedTerminates(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	_ = e.d.Handle(ctx, queued(5))
	if err := e.d.Handle(ctx, Message{Type: TypeCompleted, RunnerName: "mvm-5-1", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	if len(e.vms.terminated) != 1 {
		t.Fatal("vm not terminated")
	}
	vm, _ := e.store.GetVM(ctx, "mvm-5-1")
	if vm.Status != StatusDone {
		t.Fatalf("status %s", vm.Status)
	}
	// Hosted runners and unknown names are ignored.
	if err := e.d.Handle(ctx, Message{Type: TypeCompleted, RunnerName: "GitHub Actions 12"}); err != nil {
		t.Fatal(err)
	}
}

func TestSweeperReapsAndEnds(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	for i := int64(1); i <= 2; i++ {
		_ = e.d.Handle(ctx, queued(i))
	}
	e.gh.runners[1] = runnerState{exists: false} // job done, webhook lost
	e.gh.runners[2] = runnerState{exists: true, busy: true}
	e.now = e.now.Add(3 * time.Minute)
	res, err := e.s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ended != 1 || len(e.vms.terminated) != 1 {
		t.Fatalf("res %+v terminated %v", res, e.vms.terminated)
	}
	e.now = e.now.Add(70 * time.Minute)
	res, _ = e.s.Run(ctx)
	if res.Reaped != 1 {
		t.Fatalf("want reaped, got %+v", res)
	}
}

func TestSweeperIdleAndLost(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	_ = e.d.Handle(ctx, queued(1))
	_ = e.d.Handle(ctx, queued(2))
	e.gh.runners[1] = runnerState{exists: true, busy: false}
	e.vms.states["mvm-id-2"] = StateGone
	e.now = e.now.Add(11 * time.Minute)
	res, err := e.s.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Idle != 1 || res.Lost != 1 {
		t.Fatalf("res %+v", res)
	}
}

func TestSweeperRedispatchesLostWebhooks(t *testing.T) {
	e := newEnv()
	e.gh.queued = []github.Job{
		{ID: 10, Labels: []string{"self-hosted", "microvm"}, CreatedAt: e.now.Add(-5 * time.Minute)},
		{ID: 11, Labels: []string{"self-hosted", "microvm"}, CreatedAt: e.now.Add(-30 * time.Second)},
		{ID: 12, Labels: []string{"ubuntu-latest"}, CreatedAt: e.now.Add(-5 * time.Minute)},
	}
	res, err := e.s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Redispatched != 1 || e.queue.sent[0].JobID != 10 || e.queue.sent[0].Source != "sweeper" {
		t.Fatalf("res %+v sent %+v", res, e.queue.sent)
	}
}

func TestSweeperMessageReclaimsStaleClaim(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	_ = e.d.Handle(ctx, queued(9))
	m := queued(9)
	m.Source = "sweeper"
	_ = e.d.Handle(ctx, m)
	if len(e.vms.runs) != 1 {
		t.Fatal("fresh claim must not be re-dispatched")
	}
	e.now = e.now.Add(6 * time.Minute)
	_ = e.d.Handle(ctx, m)
	if len(e.vms.runs) != 2 || e.vms.runs[1].ClientToken != "mvm-9-2" {
		t.Fatalf("stale claim not re-dispatched: %+v", e.vms.runs)
	}
}

func TestRunnerName(t *testing.T) {
	if id, ok := ParseRunnerName(RunnerName(123, 4)); !ok || id != 123 {
		t.Fatal("round trip")
	}
	for _, bad := range []string{"", "mvm", "mvm-x-1", "runner-1-1", "mvm-1-1-1", "GitHub Actions 3"} {
		if _, ok := ParseRunnerName(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
