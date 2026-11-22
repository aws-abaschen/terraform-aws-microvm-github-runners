// Command realjob drives the sandbox's real-job test from the operator's
// machine: the test repository deploys its own runners through the tagger,
// a repository webhook points at them, and real GitHub Actions jobs run on
// MicroVMs. It also runs the automated resilience drills. It needs the AWS
// CLI credentials of the operator (admin), the GitHub CLI (`gh`, signed in)
// and git, and reads the sandbox outputs with terraform.
//
//	go run ./cmd/realjob -repo owner/repo prepare|deploy|webhook|job|drill-capacity|drill-vm-failure|drill-lost-webhooks|unhook|destroy
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvm "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type sandbox struct {
	repo, id, region string
	in               map[string]string // e2e_inputs of examples/sandbox
	cfg              aws.Config
}

func main() {
	repo := flag.String("repo", "", "test repository owner/repo")
	count := flag.Int("count", 1, "job: parallel jobs")
	sleep := flag.Int("sleep", 0, "job: seconds each job sleeps")
	regionFlag = flag.String("region", "eu-central-1", "Region when the sandbox outputs are gone")
	flag.Parse()
	if *repo == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: realjob -repo owner/repo prepare|deploy|webhook|job|drill-capacity|drill-vm-failure|drill-lost-webhooks|unhook|destroy|destroy-local")
		os.Exit(2)
	}
	ctx := context.Background()
	s := load(ctx, strings.ToLower(*repo))
	var err error
	switch flag.Arg(0) {
	case "prepare":
		err = s.prepare(ctx)
	case "deploy":
		err = s.deploy("apply")
	case "destroy":
		err = s.deploy("destroy")
	case "destroy-local":
		err = s.destroyLocal(ctx)
	case "webhook":
		err = s.webhook(ctx)
	case "unhook":
		err = s.unhook()
	case "job":
		_, err = s.job(*count, *sleep, true)
	case "drill-capacity":
		err = s.drillCapacity(ctx)
	case "drill-vm-failure":
		err = s.drillVMFailure(ctx)
	case "drill-lost-webhooks":
		err = s.drillLostWebhooks(ctx)
	default:
		err = fmt.Errorf("unknown command %q", flag.Arg(0))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

var regionFlag *string

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func load(ctx context.Context, repo string) *sandbox {
	in := map[string]string{}
	out, err := run("", "terraform", "-chdir=examples/sandbox", "output", "-json", "e2e_inputs")
	if err == nil {
		err = json.Unmarshal([]byte(out), &in)
	}
	if err != nil {
		// Sandbox outputs are gone (partial teardown): fall back to the
		// bootstrap naming convention, enough for destroy-local and unhook.
		fmt.Fprintln(os.Stderr, "no sandbox outputs, using bucket naming convention:", firstLine(err.Error()))
		in["region"] = *regionFlag
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(in["region"]))
	if err != nil {
		die("aws config: %v", err)
	}
	if in["tfstate_bucket"] == "" {
		id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			die("sts: %v", err)
		}
		acct := aws.ToString(id.Account)
		in["tfstate_bucket"] = "gha-microvm-tfstate-" + acct + "-" + in["region"]
		in["artifacts_bucket"] = "gha-microvm-artifacts-" + acct + "-" + in["region"]
	}
	sum := sha256.Sum256([]byte(repo))
	return &sandbox{repo: repo, id: hex.EncodeToString(sum[:])[:10], region: in["region"], in: in, cfg: cfg}
}

// prepare uploads the release assets and pushes the workflows, scripts and
// the consumer example (module sources pinned to this commit) to the test
// repository, and creates its runners Environment.
func (s *sandbox) prepare(ctx context.Context) error {
	s3c := s3.NewFromConfig(s.cfg)
	for _, f := range []string{"lambda.zip", "image-arm64.zip"} {
		b, err := os.ReadFile(filepath.Join("dist", f))
		if err != nil {
			return fmt.Errorf("%w (run sandbox-up first, it builds dist/)", err)
		}
		key := s.id + "/release/" + f
		if _, err := s3c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.in["artifacts_bucket"]), Key: &key, Body: bytes.NewReader(b)}); err != nil {
			return fmt.Errorf("upload %s: %w", f, err)
		}
		fmt.Printf("uploaded s3://%s/%s\n", s.in["artifacts_bucket"], key)
	}
	tmp, err := os.MkdirTemp("", "realjob-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := run("", "gh", "repo", "clone", s.repo, tmp, "--", "-q"); err != nil {
		return err
	}
	wf := filepath.Join(tmp, ".github", "workflows")
	infra := filepath.Join(tmp, "infra", "runners")
	for _, d := range []string{wf, infra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, f := range []string{"e2e.yml", "e2e-checks.sh", "deploy.yml", "job.yml", "tagger-login.sh"} {
		if err := copyFile(filepath.Join("examples", "sandbox", "e2e", f), filepath.Join(wf, f)); err != nil {
			return err
		}
	}
	// The modules of this working tree, so the cycle tests exactly what is
	// about to be committed; a consumer pins ?ref= to a release instead.
	for _, m := range []string{"runner-image", "repo-runner"} {
		dst := filepath.Join(tmp, "infra", "modules", m)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := copyDir(filepath.Join("modules", m), dst); err != nil {
			return err
		}
	}
	main, err := os.ReadFile(filepath.Join("examples", "consumer-repo", "infra", "runners", "main.tf"))
	if err != nil {
		return err
	}
	local := strings.ReplaceAll(string(main), `"../../../../modules/`, `"../modules/`)
	if err := os.WriteFile(filepath.Join(infra, "main.tf"), []byte(local), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(infra, ".gitignore"), []byte(".release/\n.terraform/\n.terraform.lock.hcl\n"), 0o644); err != nil {
		return err
	}
	if _, err := run(tmp, "git", "add", "-A"); err != nil {
		return err
	}
	if _, err := run(tmp, "git", "diff", "--cached", "--quiet"); err != nil {
		if _, err := run(tmp, "git", "commit", "-qm", "ci: sandbox workflows and runners stack"); err != nil {
			return err
		}
		if _, err := run(tmp, "git", "push", "-q"); err != nil {
			return err
		}
		fmt.Println("pushed workflows, infra/runners and infra/modules")
	}
	_, err = run("", "gh", "api", "-X", "PUT", "repos/"+s.repo+"/environments/runners")
	return err
}

func (s *sandbox) deploy(action string) error {
	start := time.Now()
	id, err := s.dispatch("deploy.yml", map[string]string{
		"region": s.region, "entry_role_arn": s.in["entry_role_arn"], "tagger_function": s.in["tagger_function"],
		"tfstate_bucket": s.in["tfstate_bucket"], "artifacts_bucket": s.in["artifacts_bucket"], "action": action,
	})
	if err != nil {
		return err
	}
	c := s.watch(id)
	fmt.Printf("deploy %s: %s in %s (run %s)\n", action, c, time.Since(start).Round(time.Second), id)
	if c != "success" {
		return fmt.Errorf("deploy %s %s: gh run view %s --repo %s --log-failed", action, c, id, s.repo)
	}
	return nil
}

// destroyLocal destroys the repository's stack from this machine with the
// operator's credentials: the fallback when the tagger path is gone or broken.
func (s *sandbox) destroyLocal(ctx context.Context) error {
	tmp, err := os.MkdirTemp("", "realjob-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, err := run("", "gh", "repo", "clone", s.repo, tmp, "--", "-q"); err != nil {
		return err
	}
	dir := filepath.Join(tmp, "infra", "runners")
	rel := filepath.Join(dir, ".release")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	s3c := s3.NewFromConfig(s.cfg)
	for _, f := range []string{"lambda.zip", "image-arm64.zip"} {
		o, err := s3c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.in["artifacts_bucket"]), Key: aws.String(s.id + "/release/" + f)})
		if err != nil {
			return fmt.Errorf("release asset %s: %w", f, err)
		}
		b, err := io.ReadAll(o.Body)
		_ = o.Body.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(rel, f), b, 0o644); err != nil {
			return err
		}
	}
	if out, err := run(dir, "terraform", "init", "-input=false", "-backend-config=bucket="+s.in["tfstate_bucket"],
		"-backend-config=key="+s.id+"/runners.tfstate", "-backend-config=region="+s.region); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	out, err := run(dir, "terraform", "destroy", "-input=false", "-auto-approve", "-no-color", "-var=region="+s.region,
		"-var=repository="+s.repo, "-var=deletion_protection=false", "-var=secret_recovery_window_days=0")
	fmt.Println(lastLines(out, 3))
	return err
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// outputs reads the consumer stack's outputs from its state object.
func (s *sandbox) outputs(ctx context.Context) (map[string]string, error) {
	o, err := s3.NewFromConfig(s.cfg).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.in["tfstate_bucket"]), Key: aws.String(s.id + "/runners.tfstate")})
	if err != nil {
		return nil, fmt.Errorf("consumer state: %w", err)
	}
	defer func() { _ = o.Body.Close() }()
	var st struct {
		Outputs map[string]struct {
			Value any `json:"value"`
		} `json:"outputs"`
	}
	if err := json.NewDecoder(o.Body).Decode(&st); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range st.Outputs {
		if str, ok := v.Value.(string); ok {
			out[k] = str
		}
	}
	return out, nil
}

func (s *sandbox) webhook(ctx context.Context) error {
	o, err := s.outputs(ctx)
	if err != nil {
		return err
	}
	sec, err := secretsmanager.NewFromConfig(s.cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(o["webhook_secret_arn"])})
	if err != nil {
		return err
	}
	if err := s.unhook(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"name": "web", "active": true, "events": []string{"workflow_job"},
		"config": map[string]string{"url": o["webhook_url"], "content_type": "json", "secret": aws.ToString(sec.SecretString), "insecure_ssl": "0"},
	})
	if _, err := runIn("", bytes.NewReader(body), "gh", "api", "-X", "POST", "repos/"+s.repo+"/hooks", "--input", "-"); err != nil {
		return err
	}
	fmt.Println("webhook created for workflow_job events")
	return nil
}

// unhook deletes the repository webhooks that point at Lambda function URLs.
func (s *sandbox) unhook() error {
	out, err := run("", "gh", "api", "repos/"+s.repo+"/hooks", "--jq", `.[] | select(.config.url | test("lambda-url")) | .id`)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(out) {
		if _, err := run("", "gh", "api", "-X", "DELETE", "repos/"+s.repo+"/hooks/"+id); err != nil {
			return err
		}
		fmt.Println("deleted webhook", id)
	}
	return nil
}

// job dispatches job.yml and, when wait is true, waits for it and prints each job.
func (s *sandbox) job(count, sleep int, wait bool) (string, error) {
	start := time.Now()
	id, err := s.dispatch("job.yml", map[string]string{"count": fmt.Sprint(count), "sleep": fmt.Sprint(sleep)})
	if err != nil || !wait {
		return id, err
	}
	c := s.watch(id)
	s.printJobs(id)
	fmt.Printf("job x%d: %s in %s (run %s)\n", count, c, time.Since(start).Round(time.Second), id)
	if c != "success" {
		return id, fmt.Errorf("job run %s: %s", id, c)
	}
	return id, nil
}

func (s *sandbox) printJobs(run string) {
	out, _ := runOut("gh", "api", "repos/"+s.repo+"/actions/runs/"+run+"/jobs", "--jq",
		`.jobs[] | select(.name | startswith("run")) | "  \(.name) \(.conclusion) runner=\(.runner_name // "-") waited=\((.started_at|fromdate) - (.created_at|fromdate))s"`)
	fmt.Print(out)
}

// drillCapacity queues more jobs than max_runners: all must complete.
func (s *sandbox) drillCapacity(ctx context.Context) error {
	fmt.Println("Drill: capacity. 7 jobs of 60s against max_runners = 5: the extra jobs wait, all must finish.")
	start := time.Now()
	if _, err := s.job(7, 60, true); err != nil {
		return err
	}
	s.metric(ctx, "Throttled", start)
	return nil
}

// drillVMFailure kills the VM of a running job: the job fails, Lost counts it,
// and a re-run succeeds.
func (s *sandbox) drillVMFailure(ctx context.Context) error {
	fmt.Println("Drill: VM failure mid-job. Terminate the VM of a running job.")
	o, err := s.outputs(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	id, err := s.job(1, 240, false)
	if err != nil {
		return err
	}
	vms := lambdamicrovms.NewFromConfig(s.cfg)
	var vm string
	for deadline := time.Now().Add(6 * time.Minute); time.Now().Before(deadline) && vm == ""; time.Sleep(10 * time.Second) {
		status, _ := runOut("gh", "run", "view", id, "--repo", s.repo, "--json", "jobs", "--jq", `[.jobs[] | select(.name | startswith("run")) | .status][0]`)
		if strings.TrimSpace(status) != "in_progress" {
			continue
		}
		l, err := vms.ListMicrovms(ctx, &lambdamicrovms.ListMicrovmsInput{ImageIdentifier: aws.String(o["image_arn"])})
		if err != nil {
			return err
		}
		for _, m := range l.Items {
			if m.State == mvm.MicrovmStateRunning {
				vm = aws.ToString(m.MicrovmId)
			}
		}
	}
	if vm == "" {
		return errors.New("no running VM found for the job")
	}
	time.Sleep(30 * time.Second) // let the job reach its sleep step
	if _, err := vms.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: &vm}); err != nil {
		return err
	}
	fmt.Println("terminated", vm)
	c := s.watch(id)
	s.printJobs(id)
	if c == "success" {
		return errors.New("the job succeeded although its VM was terminated")
	}
	fmt.Printf("PASS  job ended %s after its VM was killed\n", c)
	if _, err := run("", "gh", "run", "rerun", id, "--repo", s.repo, "--failed"); err != nil {
		return err
	}
	time.Sleep(10 * time.Second)
	if c := s.watch(id); c != "success" {
		return fmt.Errorf("re-run ended %s", c)
	}
	fmt.Println("PASS  re-run succeeded")
	time.Sleep(3 * time.Minute) // the sweeper reports Lost on its next pass
	s.metric(ctx, "Lost", start)
	return nil
}

// drillLostWebhooks removes the webhook, so GitHub's events are lost, queues
// a job and expects the sweeper to start it anyway; then restores the webhook.
func (s *sandbox) drillLostWebhooks(ctx context.Context) error {
	fmt.Println("Drill: lost webhooks. No webhook while a job is queued; the sweeper must start it within 5 minutes.")
	if err := s.unhook(); err != nil {
		return err
	}
	defer func() {
		if err := s.webhook(ctx); err != nil {
			fmt.Println("restore webhook:", err)
		}
	}()
	start := time.Now()
	id, err := s.job(1, 0, false)
	if err != nil {
		return err
	}
	c := s.watch(id)
	s.printJobs(id)
	if c != "success" {
		return fmt.Errorf("job %s without webhook", c)
	}
	fmt.Printf("PASS  job ran without its webhook, %s after dispatch\n", time.Since(start).Round(time.Second))
	s.metric(ctx, "Redispatched", start)
	return nil
}

func (s *sandbox) metric(ctx context.Context, name string, since time.Time) {
	out, err := cloudwatch.NewFromConfig(s.cfg).GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace: aws.String("MicrovmGithubRunners"), MetricName: aws.String(name),
		Dimensions: []cwtypes.Dimension{{Name: aws.String("Repository"), Value: aws.String(s.repo)}},
		StartTime:  aws.Time(since.Add(-time.Minute)), EndTime: aws.Time(time.Now().Add(time.Minute)),
		Period: aws.Int32(3600), Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
	})
	if err != nil {
		fmt.Printf("metric %s: %v\n", name, err)
		return
	}
	sum := 0.0
	for _, d := range out.Datapoints {
		sum += aws.ToFloat64(d.Sum)
	}
	fmt.Printf("metric %s since start: %.0f\n", name, sum)
}

// dispatch starts a workflow and returns the id of the run it created.
func (s *sandbox) dispatch(wf string, inputs map[string]string) (string, error) {
	before, _ := runOut("gh", "run", "list", "--repo", s.repo, "--workflow", wf, "--limit", "1", "--json", "databaseId", "--jq", ".[0].databaseId")
	args := []string{"workflow", "run", wf, "--repo", s.repo}
	for k, v := range inputs {
		args = append(args, "-f", k+"="+v)
	}
	if _, err := run("", "gh", args...); err != nil {
		return "", err
	}
	for i := 0; i < 30; i++ {
		time.Sleep(3 * time.Second)
		id, _ := runOut("gh", "run", "list", "--repo", s.repo, "--workflow", wf, "--limit", "1", "--json", "databaseId", "--jq", ".[0].databaseId")
		if id = strings.TrimSpace(id); id != "" && id != strings.TrimSpace(before) {
			return id, nil
		}
	}
	return "", fmt.Errorf("no new run of %s", wf)
}

// watch waits for a run to finish and returns its conclusion. A run still
// open after 40 minutes (for example a job no runner ever picks up) is
// cancelled, so nothing waits forever.
func (s *sandbox) watch(id string) string {
	for deadline := time.Now().Add(40 * time.Minute); ; {
		if time.Now().After(deadline) {
			_, _ = run("", "gh", "run", "cancel", id, "--repo", s.repo)
			fmt.Printf("run %s still open after 40 minutes: cancelled\n", id)
			deadline = time.Now().Add(5 * time.Minute)
		}
		out, _ := runOut("gh", "run", "view", id, "--repo", s.repo, "--json", "status,conclusion", "--jq", `.status + " " + .conclusion`)
		f := strings.Fields(out)
		if len(f) == 2 && f[0] == "completed" {
			return f[1]
		}
		time.Sleep(10 * time.Second)
	}
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if strings.HasSuffix(p, ".tf") {
			return copyFile(p, filepath.Join(dst, rel))
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func run(dir, name string, args ...string) (string, error) { return runIn(dir, nil, name, args...) }

func runOut(name string, args ...string) (string, error) { return runIn("", nil, name, args...) }

func runIn(dir string, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(redact(args), " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// redact keeps secrets passed as arguments out of error messages.
func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(strings.ToLower(a), "secret") && strings.Contains(a, "=") {
			a = a[:strings.Index(a, "=")+1] + "***"
		}
		out[i] = a
	}
	return out
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
