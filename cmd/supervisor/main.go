//go:build linux

// Command supervisor runs inside the MicroVM. It serves the Lambda MicroVM
// lifecycle hooks, fetches the one-time JIT config, starts dockerd and runs
// exactly one GitHub Actions job.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const hookBase = "/aws/lambda-microvms/runtime/v1/"

type supervisor struct {
	log       *slog.Logger
	runnerDir string
	user      string
	docker    bool

	mu      sync.Mutex
	started bool
	cmd     *exec.Cmd
}

type runRequest struct {
	MicrovmID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}

type payload struct {
	Param  string `json:"param"`
	Region string `json:"region"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "supervisor")
	s := &supervisor{
		log:       log,
		runnerDir: getenv("RUNNER_DIR", "/home/runner/actions-runner"),
		user:      getenv("RUNNER_USER", "runner"),
		docker:    os.Getenv("ENABLE_DOCKER") == "true",
	}
	if err := s.selfCheck(); err != nil {
		// Fail the image build loudly: /ready never turns 200.
		log.Error("self check failed", "err", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+hookBase+"ready", s.ready)
	mux.HandleFunc("POST "+hookBase+"validate", s.ready)
	mux.HandleFunc("POST "+hookBase+"run", s.run)
	mux.HandleFunc("POST "+hookBase+"resume", ok)
	mux.HandleFunc("POST "+hookBase+"suspend", ok)
	mux.HandleFunc("POST "+hookBase+"terminate", s.terminate)
	addr := ":" + getenv("HOOK_PORT", "9000")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

var selfCheckErr error

func (s *supervisor) selfCheck() error {
	if _, err := os.Stat(filepath.Join(s.runnerDir, "run.sh")); err != nil {
		selfCheckErr = fmt.Errorf("runner not installed: %w", err)
		return selfCheckErr
	}
	if _, err := user.Lookup(s.user); err != nil {
		selfCheckErr = fmt.Errorf("runner user: %w", err)
		return selfCheckErr
	}
	return nil
}

func (s *supervisor) ready(w http.ResponseWriter, _ *http.Request) {
	if selfCheckErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *supervisor) run(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		// A MicroVM serves exactly one job.
		w.WriteHeader(http.StatusOK)
		return
	}
	var req runRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad run request", http.StatusBadRequest)
		return
	}
	var p payload
	if err := json.Unmarshal([]byte(req.RunHookPayload), &p); err != nil || p.Param == "" {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	log := s.log.With("microvm_id", req.MicrovmID, "param", p.Param)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	jit, err := fetchOnce(ctx, p)
	if err != nil {
		log.Error("fetch jit config", "err", err)
		http.Error(w, "jit config unavailable", http.StatusInternalServerError)
		return
	}
	raiseNoFile(log)
	if s.docker {
		go startDocker(log)
	}
	cmd, err := s.startRunner(jit)
	if err != nil {
		log.Error("start runner", "err", err)
		http.Error(w, "runner failed to start", http.StatusInternalServerError)
		return
	}
	s.cmd, s.started = cmd, true
	go func() {
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		log.Info("runner exited", "exit_code", code)
	}()
	log.Info("runner started")
	w.WriteHeader(http.StatusOK)
}

// fetchOnce reads the JIT config and deletes it, so it cannot be replayed.
func fetchOnce(ctx context.Context, p payload) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(p.Region))
	if err != nil {
		return "", err
	}
	c := ssm.NewFromConfig(cfg)
	out, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: &p.Param, WithDecryption: aws.Bool(true)})
	if err != nil {
		return "", err
	}
	if _, err := c.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: &p.Param}); err != nil {
		return "", fmt.Errorf("delete jit param: %w", err)
	}
	return aws.ToString(out.Parameter.Value), nil
}

func (s *supervisor) startRunner(jit string) (*exec.Cmd, error) {
	u, err := user.Lookup(s.user)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	groups := []uint32{}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if v, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(v))
			}
		}
	}
	cmd := exec.Command(filepath.Join(s.runnerDir, "run.sh"), "--jitconfig", jit)
	cmd.Dir = s.runnerDir
	cmd.Env = []string{"HOME=" + u.HomeDir, "USER=" + s.user, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8"}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups},
	}
	return cmd, cmd.Start()
}

func (s *supervisor) terminate(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	s.log.Info("terminated")
	w.WriteHeader(http.StatusOK)
}

func raiseNoFile(log *slog.Logger) {
	lim := syscall.Rlimit{Cur: 1048576, Max: 1048576}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		log.Warn("raise nofile", "err", err)
	}
}

// startDocker runs dockerd per job (never inside the snapshot). The VPC
// resolver is used so private endpoint zones resolve inside containers.
func startDocker(log *slog.Logger) {
	cmd := exec.Command("/usr/bin/dockerd",
		"--dns", "169.254.169.253",
		"--storage-driver", "overlay2",
		"--log-driver", "json-file")
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Error("start dockerd", "err", err)
		return
	}
	for i := 0; i < 60; i++ {
		if _, err := os.Stat("/var/run/docker.sock"); err == nil {
			_ = os.Chmod("/var/run/docker.sock", 0o660)
			log.Info("dockerd ready")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Error("dockerd not ready after 30s")
}
