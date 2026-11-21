// Command smoke checks a deployed sandbox without GitHub: it launches one
// MicroVM exactly like the dispatcher does and verifies that the VM starts
// with NO_INGRESS, that its role and the VPC path reach SSM (the supervisor
// reads and deletes the one-time parameter) and that it terminates.
//
//	terraform -chdir=examples/sandbox output -json smoke | go run ./cmd/smoke -region eu-central-1
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvm "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type outputs struct {
	ImageArn      string `json:"image_arn"`
	RunnerRoleArn string `json:"runner_role_arn"`
	ConnectorArn  string `json:"connector_arn"`
	KMSKeyArn     string `json:"kms_key_arn"`
	LogGroup      string `json:"log_group"`
	ParamPrefix   string `json:"param_prefix"`
}

type result struct {
	name string
	ok   bool
	note string
}

func main() {
	region := flag.String("region", "eu-central-1", "AWS Region")
	flag.Parse()

	var o outputs
	if err := json.NewDecoder(os.Stdin).Decode(&o); err != nil {
		fail("read terraform output from stdin: %v", err)
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		fail("aws config: %v", err)
	}
	vms := lambdamicrovms.NewFromConfig(cfg)
	params := ssm.NewFromConfig(cfg)
	var results []result
	check := func(name string, ok bool, note string, a ...any) {
		results = append(results, result{name, ok, fmt.Sprintf(note, a...)})
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		fmt.Printf("%s  %-34s %s\n", mark, name, fmt.Sprintf(note, a...))
	}

	name := fmt.Sprintf("smoke-%d", time.Now().Unix())
	param := o.ParamPrefix + "/jit/" + name
	_, err = params.PutParameter(ctx, &ssm.PutParameterInput{
		Name: &param, Value: aws.String("not-a-real-jit-config"), Type: ssmtypes.ParameterTypeSecureString,
		KeyId: &o.KMSKeyArn, Tier: ssmtypes.ParameterTierIntelligentTiering, Overwrite: aws.Bool(true),
	})
	check("put one-time parameter", err == nil, "%v", errText(err))
	if err != nil {
		os.Exit(1)
	}

	noIngress := fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:NO_INGRESS", *region)
	payload, _ := json.Marshal(map[string]string{"param": param, "region": *region})
	start := time.Now()
	out, err := vms.RunMicrovm(ctx, &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier:          &o.ImageArn,
		ClientToken:              &name,
		ExecutionRoleArn:         &o.RunnerRoleArn,
		EgressNetworkConnectors:  []string{o.ConnectorArn},
		IngressNetworkConnectors: []string{noIngress},
		KmsKeyIdentifier:         &o.KMSKeyArn,
		MaximumDurationInSeconds: aws.Int32(600),
		RunHookPayload:           aws.String(string(payload)),
		Logging:                  &mvm.LoggingMemberCloudWatch{Value: mvm.CloudWatchLogging{LogGroup: &o.LogGroup}},
	})
	check("RunMicrovm (VPC egress, NO_INGRESS, KMS)", err == nil, "%v", errText(err))
	if err != nil {
		_, _ = params.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: &param})
		summary(results)
	}
	id := aws.ToString(out.MicrovmId)
	defer terminate(ctx, vms, id)

	state, reason := waitState(ctx, vms, id, 5*time.Minute, mvm.MicrovmStateRunning)
	check("VM reaches RUNNING (run hook ok)", state == mvm.MicrovmStateRunning, "state=%s after %s %s", state, time.Since(start).Round(time.Second), reason)

	deleted := false
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		_, err := params.GetParameter(ctx, &ssm.GetParameterInput{Name: &param})
		var nf *ssmtypes.ParameterNotFound
		if errors.As(err, &nf) {
			deleted = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	check("supervisor read+deleted parameter", deleted, "proves execution role credentials, KMS decrypt and VPC path to SSM")
	if !deleted {
		_, _ = params.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: &param})
	}

	terminate(ctx, vms, id)
	state, _ = waitState(ctx, vms, id, 3*time.Minute, mvm.MicrovmStateTerminated)
	check("TerminateMicrovm", state == mvm.MicrovmStateTerminated || state == "GONE", "state=%s", state)
	fmt.Printf("\nVM logs: aws logs tail %s --region %s --since 15m\n", o.LogGroup, *region)
	summary(results)
}

func waitState(ctx context.Context, c *lambdamicrovms.Client, id string, max time.Duration, want mvm.MicrovmState) (mvm.MicrovmState, string) {
	deadline := time.Now().Add(max)
	var last mvm.MicrovmState
	var reason string
	for time.Now().Before(deadline) {
		out, err := c.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: &id})
		var nf *mvm.ResourceNotFoundException
		if errors.As(err, &nf) {
			return "GONE", ""
		}
		if err == nil {
			last, reason = out.State, aws.ToString(out.StateReason)
			if last == want || (last == mvm.MicrovmStateTerminated && want != last) {
				return last, reason
			}
		}
		time.Sleep(5 * time.Second)
	}
	return last, reason
}

func terminate(ctx context.Context, c *lambdamicrovms.Client, id string) {
	if id != "" {
		_, _ = c.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: &id})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func summary(results []result) {
	failed := 0
	for _, r := range results {
		if !r.ok {
			failed++
		}
	}
	fmt.Printf("\n%d checks, %d failed\n", len(results), failed)
	if failed > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}
