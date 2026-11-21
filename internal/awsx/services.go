package awsx

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvmtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/runner"
)

// MicroVMs implements runner.MicroVMs.
type MicroVMs struct {
	Client           *lambdamicrovms.Client
	Image            string
	ImageVersion     string
	ExecutionRoleArn string
	Egress           []string
	Ingress          []string
	KMSKeyArn        string
	LogGroup         string
	MaxDurationSec   int32
}

var _ runner.MicroVMs = (*MicroVMs)(nil)

// Run implements runner.MicroVMs.
func (m *MicroVMs) Run(ctx context.Context, in runner.RunInput) (string, error) {
	req := &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier:          &m.Image,
		ClientToken:              &in.ClientToken,
		ExecutionRoleArn:         &m.ExecutionRoleArn,
		EgressNetworkConnectors:  m.Egress,
		IngressNetworkConnectors: m.Ingress,
		MaximumDurationInSeconds: &m.MaxDurationSec,
		RunHookPayload:           &in.Payload,
		Logging:                  &mvmtypes.LoggingMemberCloudWatch{Value: mvmtypes.CloudWatchLogging{LogGroup: &m.LogGroup}},
	}
	if m.ImageVersion != "" {
		req.ImageVersion = &m.ImageVersion
	}
	if m.KMSKeyArn != "" {
		req.KmsKeyIdentifier = &m.KMSKeyArn
	}
	out, err := m.Client.RunMicrovm(ctx, req)
	if err != nil {
		return "", err
	}
	return aws.ToString(out.MicrovmId), nil
}

func notFound(err error) bool {
	var nf *mvmtypes.ResourceNotFoundException
	return errors.As(err, &nf)
}

// State implements runner.MicroVMs.
func (m *MicroVMs) State(ctx context.Context, id string) (string, error) {
	out, err := m.Client.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: &id})
	if notFound(err) {
		return runner.StateGone, nil
	}
	if err != nil {
		return "", err
	}
	switch out.State {
	case mvmtypes.MicrovmStateTerminated, mvmtypes.MicrovmStateTerminating:
		return runner.StateGone, nil
	}
	return string(out.State), nil
}

// Terminate implements runner.MicroVMs.
func (m *MicroVMs) Terminate(ctx context.Context, id string) error {
	_, err := m.Client.TerminateMicrovm(ctx, &lambdamicrovms.TerminateMicrovmInput{MicrovmIdentifier: &id})
	if notFound(err) {
		return nil
	}
	return err
}

// Params implements runner.Params on SSM Parameter Store.
type Params struct {
	Client    *ssm.Client
	KMSKeyArn string
}

var _ runner.Params = (*Params)(nil)

// Put implements runner.Params.
func (p *Params) Put(ctx context.Context, name, value string) error {
	_, err := p.Client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      &name,
		Value:     &value,
		Type:      ssmtypes.ParameterTypeSecureString,
		KeyId:     &p.KMSKeyArn,
		Tier:      ssmtypes.ParameterTierIntelligentTiering,
		Overwrite: aws.Bool(true),
	})
	return err
}

// Delete implements runner.Params.
func (p *Params) Delete(ctx context.Context, name string) error {
	_, err := p.Client.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: &name})
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// Queue implements runner.Queue on SQS.
type Queue struct {
	Client *sqs.Client
	URL    string
}

var _ runner.Queue = (*Queue)(nil)

// Send implements runner.Queue.
func (q *Queue) Send(ctx context.Context, m runner.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = q.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &q.URL, MessageBody: aws.String(string(b))})
	return err
}

// SecretString reads a secret value.
func SecretString(ctx context.Context, c *secretsmanager.Client, arn string) ([]byte, error) {
	out, err := c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
	if err != nil {
		return nil, err
	}
	return []byte(aws.ToString(out.SecretString)), nil
}
