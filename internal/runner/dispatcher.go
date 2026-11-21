package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/labels"
)

// Dispatcher launches one MicroVM per queued job and terminates it when the
// job completes.
type Dispatcher struct {
	Cfg    Config
	Store  Store
	VMs    MicroVMs
	Params Params
	GitHub GitHub
	Log    *slog.Logger
	Now    func() time.Time
}

// RunPayload is passed to the MicroVM /run hook. It holds a pointer, never the secret.
type RunPayload struct {
	Param  string `json:"param"`
	Region string `json:"region"`
}

// Handle processes one queue message.
func (d *Dispatcher) Handle(ctx context.Context, m Message) error {
	switch m.Type {
	case TypeQueued:
		return d.queued(ctx, m)
	case TypeCompleted:
		return d.completed(ctx, m)
	default:
		d.Log.Warn("unknown message type", "type", m.Type)
		return nil
	}
}

func (d *Dispatcher) queued(ctx context.Context, m Message) error {
	log := d.Log.With("job_id", m.JobID, "run_id", m.RunID, "source", m.Source, "delivery", m.Delivery)
	if !labels.Match(m.Labels, d.Cfg.RunnerLabels, d.Cfg.RequiredLabels) {
		log.Info("labels do not match, ignoring", "labels", m.Labels)
		return nil
	}
	now := d.Now()
	var staleBefore time.Time
	if m.Source == "sweeper" {
		staleBefore = now.Add(-d.Cfg.ClaimStaleAfter)
	}
	attempt, ok, err := d.Store.Claim(ctx, m.JobID, now, staleBefore)
	if err != nil {
		return fmt.Errorf("claim job: %w", err)
	}
	if !ok {
		log.Info("job already claimed, skipping duplicate")
		return nil
	}

	active, err := d.Store.ActiveVMs(ctx)
	if err != nil {
		_ = d.Store.ReleaseClaim(ctx, m.JobID)
		return fmt.Errorf("count active: %w", err)
	}
	if len(active) >= d.Cfg.MaxRunners {
		_ = d.Store.ReleaseClaim(ctx, m.JobID)
		log.Warn("runner cap reached, will retry", "active", len(active), "max", d.Cfg.MaxRunners)
		return ErrThrottled
	}

	name := RunnerName(m.JobID, attempt)
	token, err := d.GitHub.RepoToken(ctx, d.Cfg.Owner, d.Cfg.Repo)
	if err != nil {
		_ = d.Store.ReleaseClaim(ctx, m.JobID)
		return fmt.Errorf("installation token: %w", err)
	}
	jit, err := d.GitHub.GenerateJITConfig(ctx, token, d.Cfg.Owner, d.Cfg.Repo, name, d.Cfg.RunnerGroupID, d.Cfg.RunnerLabels)
	if err != nil {
		_ = d.Store.ReleaseClaim(ctx, m.JobID)
		return fmt.Errorf("generate jit config: %w", err)
	}
	param := d.Cfg.ParamName(name)
	cleanup := func() {
		_ = d.Params.Delete(ctx, param)
		_ = d.GitHub.DeleteRunner(ctx, token, d.Cfg.Owner, d.Cfg.Repo, jit.RunnerID)
		_ = d.Store.ReleaseClaim(ctx, m.JobID)
	}
	if err := d.Params.Put(ctx, param, jit.Encoded); err != nil {
		cleanup()
		return fmt.Errorf("store jit config: %w", err)
	}
	payload, _ := json.Marshal(RunPayload{Param: param, Region: d.Cfg.Region})
	id, err := d.VMs.Run(ctx, RunInput{ClientToken: name, Payload: string(payload)})
	if err != nil {
		cleanup()
		return fmt.Errorf("run microvm: %w", err)
	}
	vm := VM{RunnerName: name, RunnerID: jit.RunnerID, MicrovmID: id, JobID: m.JobID, Status: StatusRunning, StartedAt: now}
	if err := d.Store.PutVM(ctx, vm); err != nil {
		// The VM runs untracked; terminate it so nothing leaks.
		_ = d.VMs.Terminate(ctx, id)
		cleanup()
		return fmt.Errorf("record vm: %w", err)
	}
	log.Info("audit: runner launched", "audit", true, "event", "runner.launched",
		"runner_name", name, "runner_id", jit.RunnerID, "microvm_id", id, "attempt", attempt,
		"actor", m.Actor, "workflow", m.Workflow, "head_sha", m.HeadSHA, "head_branch", m.HeadBranch)
	return nil
}

func (d *Dispatcher) completed(ctx context.Context, m Message) error {
	log := d.Log.With("job_id", m.JobID, "runner_name", m.RunnerName, "delivery", m.Delivery)
	if _, ok := ParseRunnerName(m.RunnerName); !ok {
		return nil // not one of ours, or cancelled before assignment
	}
	vm, err := d.Store.GetVM(ctx, m.RunnerName)
	if err != nil {
		return fmt.Errorf("get vm: %w", err)
	}
	if vm == nil || vm.Status != StatusRunning {
		return nil
	}
	if err := d.VMs.Terminate(ctx, vm.MicrovmID); err != nil {
		return fmt.Errorf("terminate %s: %w", vm.MicrovmID, err)
	}
	_ = d.Params.Delete(ctx, d.Cfg.ParamName(vm.RunnerName))
	if err := d.Store.EndVM(ctx, vm.RunnerName, StatusDone, d.Now()); err != nil {
		return fmt.Errorf("end vm: %w", err)
	}
	log.Info("audit: runner terminated", "audit", true, "event", "runner.terminated",
		"microvm_id", vm.MicrovmID, "reason", "job_completed", "conclusion", m.Conclusion,
		"duration_s", int(d.Now().Sub(vm.StartedAt).Seconds()))
	return nil
}
