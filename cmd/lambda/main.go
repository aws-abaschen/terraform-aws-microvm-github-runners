// Command lambda is a single binary serving the webhook, dispatcher and
// sweeper functions. FUNCTION_ROLE selects which one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/awsx"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/broker"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/github"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/labels"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/obs"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/runner"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/tagger"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/webhook"
)

func env(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required env %s\n", k)
		os.Exit(1)
	}
	return v
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Error("aws config", "err", err)
		os.Exit(1)
	}
	switch os.Getenv("FUNCTION_ROLE") {
	case "broker":
		runBroker(ctx, awsCfg, log)
		return
	case "tagger":
		runTagger(awsCfg, log)
		return
	}
	repoFull := env("REPOSITORY")
	owner, repo, ok := strings.Cut(repoFull, "/")
	if !ok {
		log.Error("REPOSITORY must be owner/repo")
		os.Exit(1)
	}
	log = log.With("repository", repoFull, "function_role", os.Getenv("FUNCTION_ROLE"))
	runnerLabels := labels.Split(env("RUNNER_LABELS"))
	requiredLabels := labels.Split(os.Getenv("REQUIRED_LABELS"))
	sm := secretsmanager.NewFromConfig(awsCfg)
	q := &awsx.Queue{Client: sqs.NewFromConfig(awsCfg), URL: os.Getenv("QUEUE_URL")}

	switch os.Getenv("FUNCTION_ROLE") {
	case "webhook":
		secret := cachedSecret(sm, env("WEBHOOK_SECRET_ARN"), 5*time.Minute)
		h := &webhook.Handler{Secret: secret, Queue: q, Repository: repoFull, RunnerLabels: runnerLabels, RequiredLabels: requiredLabels, Log: log}
		lambda.Start(h.Handle)

	case "dispatcher", "sweeper":
		app := &awsx.BrokeredGitHub{
			App:      github.NewClient(env("GITHUB_API_URL")),
			Lambda:   awslambda.NewFromConfig(awsCfg),
			Presign:  sts.NewPresignClient(sts.NewFromConfig(awsCfg)),
			Function: env("BROKER_FUNCTION"),
		}
		maxDur := time.Duration(envInt("MAX_DURATION_SECONDS", 3900)) * time.Second
		cfg := runner.Config{
			Owner: owner, Repo: repo, Region: awsCfg.Region,
			RunnerLabels: runnerLabels, RequiredLabels: requiredLabels,
			RunnerGroupID:   int64(envInt("RUNNER_GROUP_ID", 1)),
			MaxRunners:      envInt("MAX_RUNNERS", 5),
			MaxDuration:     maxDur,
			ParamPrefix:     env("PARAM_PREFIX"),
			IdleTimeout:     time.Duration(envInt("IDLE_TIMEOUT_SECONDS", 600)) * time.Second,
			ClaimStaleAfter: 5 * time.Minute,
			QueuedGrace:     2 * time.Minute,
		}
		store := &awsx.Store{DB: dynamodb.NewFromConfig(awsCfg), Table: env("TABLE_NAME")}
		vms := &awsx.MicroVMs{
			Client:           lambdamicrovms.NewFromConfig(awsCfg),
			Image:            env("IMAGE_ARN"),
			ImageVersion:     os.Getenv("IMAGE_VERSION"),
			ExecutionRoleArn: env("RUNNER_ROLE_ARN"),
			Egress:           labels.Split(env("EGRESS_CONNECTORS")),
			Ingress:          labels.Split(env("INGRESS_CONNECTORS")),
			KMSKeyArn:        os.Getenv("MICROVM_KMS_KEY_ARN"),
			LogGroup:         env("MICROVM_LOG_GROUP"),
			MaxDurationSec:   int32(maxDur.Seconds()),
		}
		params := &awsx.Params{Client: ssm.NewFromConfig(awsCfg), KMSKeyArn: env("KMS_KEY_ARN")}

		if os.Getenv("FUNCTION_ROLE") == "dispatcher" {
			d := &runner.Dispatcher{Cfg: cfg, Store: store, VMs: vms, Params: params, GitHub: app, Log: log, Now: time.Now}
			lambda.Start(func(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
				var out events.SQSEventResponse
				for _, rec := range ev.Records {
					var m runner.Message
					if err := json.Unmarshal([]byte(rec.Body), &m); err != nil {
						log.Error("drop malformed message", "message_id", rec.MessageId, "err", err)
						continue
					}
					if err := d.Handle(ctx, m); err != nil {
						if errors.Is(err, runner.ErrThrottled) {
							obs.Metric(log, "Throttled", 1)
						} else {
							obs.Metric(log, "DispatchErrors", 1)
							log.Error("dispatch failed", "message_id", rec.MessageId, "job_id", m.JobID, "err", err)
						}
						out.BatchItemFailures = append(out.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: rec.MessageId})
						continue
					}
					obs.Metric(log, "Dispatched_"+m.Type, 1)
				}
				return out, nil
			})
		}
		s := &runner.Sweeper{Cfg: cfg, Store: store, VMs: vms, Params: params, GitHub: app, Queue: q, Log: log, Now: time.Now}
		lambda.Start(func(ctx context.Context) error {
			res, err := s.Run(ctx)
			obs.Metrics(log, map[string]float64{
				"ActiveRunners": float64(res.Active), "Reaped": float64(res.Reaped), "IdleTerminated": float64(res.Idle),
				"Lost": float64(res.Lost), "EndedBySweeper": float64(res.Ended), "Redispatched": float64(res.Redispatched),
			})
			if err != nil {
				obs.Metric(log, "SweepErrors", 1)
				log.Error("sweep", "err", err)
			}
			return err
		})

	default:
		log.Error("FUNCTION_ROLE must be webhook, dispatcher, sweeper, broker or tagger")
		os.Exit(1)
	}
}

// runBroker serves the platform token broker. It is the only function that
// can read the GitHub App private key.
func runBroker(ctx context.Context, awsCfg aws.Config, log *slog.Logger) {
	log = log.With("function_role", "broker")
	raw, err := awsx.SecretString(ctx, secretsmanager.NewFromConfig(awsCfg), env("APP_SECRET_ARN"))
	if err != nil {
		log.Error("read app secret", "err", err)
		os.Exit(1)
	}
	app, err := github.NewApp(raw, env("GITHUB_API_URL"), nil)
	if err != nil {
		log.Error("github app", "err", err)
		os.Exit(1)
	}
	b := &broker.Broker{
		Prefix: env("NAME_PREFIX"), Account: env("ACCOUNT_ID"), Region: awsCfg.Region, Partition: env("PARTITION"),
		Registry: &awsx.Registry{Client: ssm.NewFromConfig(awsCfg), Prefix: env("REGISTRY_PREFIX")}, Tokens: app, HTTP: &http.Client{Timeout: 10 * time.Second}, Log: log, Now: time.Now,
	}
	lambda.Start(b.Handle)
}

// runTagger serves the platform tagger: GitHub OIDC token in, a session of
// the shared deploy role tagged with the repository out.
func runTagger(awsCfg aws.Config, log *slog.Logger) {
	log = log.With("function_role", "tagger")
	t := &tagger.Tagger{
		Verifier: &tagger.Verifier{
			Issuer: env("OIDC_ISSUER"), Audience: env("TAGGER_AUDIENCE"),
			HTTP: &http.Client{Timeout: 5 * time.Second}, Now: time.Now,
		},
		Owners:   ownerIDs(env("ALLOWED_OWNERS")),
		Registry: &awsx.Registry{Client: ssm.NewFromConfig(awsCfg), Prefix: env("REGISTRY_PREFIX")},
		Replay:   &awsx.JTIStore{DB: dynamodb.NewFromConfig(awsCfg), Table: env("JTI_TABLE")},
		STS:      &awsx.DeployRole{STS: sts.NewFromConfig(awsCfg), RoleARN: env("DEPLOY_ROLE_ARN")},
		Duration: time.Duration(envInt("SESSION_SECONDS", 3600)) * time.Second,
		Log:      log,
	}
	lambda.Start(t.Handle)
}

// ownerIDs parses ALLOWED_OWNERS, "owner:id,owner:id", into lower-case owner => id.
func ownerIDs(s string) map[string]string {
	out := map[string]string{}
	for _, p := range labels.Split(s) {
		o, id, ok := strings.Cut(p, ":")
		if !ok || id == "" {
			fmt.Fprintf(os.Stderr, "ALLOWED_OWNERS entry %q must be owner:id\n", p)
			os.Exit(1)
		}
		out[strings.ToLower(o)] = id
	}
	return out
}

func cachedSecret(sm *secretsmanager.Client, arn string, ttl time.Duration) func(context.Context) ([]byte, error) {
	var (
		mu      sync.Mutex
		val     []byte
		fetched time.Time
	)
	return func(ctx context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if val != nil && time.Since(fetched) < ttl {
			return val, nil
		}
		v, err := awsx.SecretString(ctx, sm, arn)
		if err != nil {
			return nil, err
		}
		val, fetched = v, time.Now()
		return val, nil
	}
}
