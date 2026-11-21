package runner

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/labels"
)

// Sweeper repairs what webhooks miss: lost deliveries, idle runners, VMs
// past their lifetime and VMs that died.
type Sweeper struct {
	Cfg    Config
	Store  Store
	VMs    MicroVMs
	Params Params
	GitHub GitHub
	Queue  Queue
	Log    *slog.Logger
	Now    func() time.Time
}

// Result counts what one sweep did. It is logged as metrics.
type Result struct {
	Active, Ended, Reaped, Idle, Lost, Redispatched int
}

// Run performs one sweep.
func (s *Sweeper) Run(ctx context.Context) (Result, error) {
	var res Result
	token, err := s.GitHub.RepoToken(ctx, s.Cfg.Owner, s.Cfg.Repo)
	if err != nil {
		return res, err
	}
	vms, err := s.Store.ActiveVMs(ctx)
	if err != nil {
		return res, err
	}
	res.Active = len(vms)
	var errs []error
	for _, vm := range vms {
		if err := s.checkVM(ctx, token, vm, &res); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.redispatch(ctx, token, &res); err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

func (s *Sweeper) checkVM(ctx context.Context, token string, vm VM, res *Result) error {
	now := s.Now()
	age := now.Sub(vm.StartedAt)
	log := s.Log.With("runner_name", vm.RunnerName, "microvm_id", vm.MicrovmID, "age_s", int(age.Seconds()))

	state, err := s.VMs.State(ctx, vm.MicrovmID)
	if err != nil {
		return err
	}
	if state == StateGone {
		res.Lost++
		_ = s.Params.Delete(ctx, s.Cfg.ParamName(vm.RunnerName))
		_ = s.GitHub.DeleteRunner(ctx, token, s.Cfg.Owner, s.Cfg.Repo, vm.RunnerID)
		log.Info("audit: runner vm gone", "audit", true, "event", "runner.lost")
		return s.Store.EndVM(ctx, vm.RunnerName, StatusLost, now)
	}

	if age > s.Cfg.MaxDuration+time.Minute {
		res.Reaped++
		log.Warn("audit: runner reaped past max lifetime", "audit", true, "event", "runner.reaped")
		return s.end(ctx, token, vm, StatusReaped)
	}

	exists, busy, err := s.GitHub.RunnerStatus(ctx, token, s.Cfg.Owner, s.Cfg.Repo, vm.RunnerID)
	if err != nil {
		return err
	}
	switch {
	case !exists && age > 2*time.Minute:
		// JIT runner removed by GitHub: its job is done but we missed "completed".
		res.Ended++
		log.Info("audit: runner terminated", "audit", true, "event", "runner.terminated", "reason", "runner_deregistered")
		return s.end(ctx, token, vm, StatusDone)
	case exists && !busy && age > s.Cfg.IdleTimeout:
		res.Idle++
		log.Info("audit: runner terminated", "audit", true, "event", "runner.terminated", "reason", "idle")
		return s.end(ctx, token, vm, StatusIdle)
	}
	return nil
}

func (s *Sweeper) end(ctx context.Context, token string, vm VM, status string) error {
	if err := s.VMs.Terminate(ctx, vm.MicrovmID); err != nil {
		return err
	}
	_ = s.Params.Delete(ctx, s.Cfg.ParamName(vm.RunnerName))
	_ = s.GitHub.DeleteRunner(ctx, token, s.Cfg.Owner, s.Cfg.Repo, vm.RunnerID)
	return s.Store.EndVM(ctx, vm.RunnerName, status, s.Now())
}

func (s *Sweeper) redispatch(ctx context.Context, token string, res *Result) error {
	jobs, err := s.GitHub.QueuedJobs(ctx, token, s.Cfg.Owner, s.Cfg.Repo, 30)
	if err != nil {
		return err
	}
	now := s.Now()
	for _, j := range jobs {
		if now.Sub(j.CreatedAt) < s.Cfg.QueuedGrace || !labels.Match(j.Labels, s.Cfg.RunnerLabels, s.Cfg.RequiredLabels) {
			continue
		}
		// The dispatcher's claim decides whether this is really needed.
		if err := s.Queue.Send(ctx, Message{Type: TypeQueued, Source: "sweeper", JobID: j.ID, RunID: j.RunID, Labels: j.Labels}); err != nil {
			return err
		}
		res.Redispatched++
	}
	return nil
}
