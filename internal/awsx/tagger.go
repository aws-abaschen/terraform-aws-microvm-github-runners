package awsx

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/tagger"
)

// JTIStore implements tagger.Replay with a DynamoDB table keyed by jti and a
// TTL on expires_at.
type JTIStore struct {
	DB    *dynamodb.Client
	Table string
}

var _ tagger.Replay = (*JTIStore)(nil)

// Claim implements tagger.Replay.
func (s *JTIStore) Claim(ctx context.Context, jti string, until time.Time) (bool, error) {
	_, err := s.DB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &s.Table,
		Item: map[string]ddbtypes.AttributeValue{
			"jti":        &ddbtypes.AttributeValueMemberS{Value: jti},
			"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(until.Add(time.Hour).Unix(), 10)},
		},
		ConditionExpression: aws.String("attribute_not_exists(jti)"),
	})
	var cf *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &cf) {
		return false, nil
	}
	return err == nil, err
}

// DeployRole implements tagger.Assumer on the shared deploy role.
type DeployRole struct {
	STS     *sts.Client
	RoleARN string
}

var _ tagger.Assumer = (*DeployRole)(nil)

// Assume implements tagger.Assumer.
func (d *DeployRole) Assume(ctx context.Context, in tagger.AssumeInput) (tagger.Response, error) {
	tags := make([]ststypes.Tag, 0, len(in.Tags))
	for k, v := range in.Tags {
		tags = append(tags, ststypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	out, err := d.STS.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         &d.RoleARN,
		RoleSessionName: &in.SessionName,
		SourceIdentity:  &in.SourceIdentity,
		Tags:            tags,
		DurationSeconds: aws.Int32(int32(in.Duration.Seconds())),
	})
	if err != nil {
		return tagger.Response{}, err
	}
	c := out.Credentials
	return tagger.Response{
		AccessKeyID: aws.ToString(c.AccessKeyId), SecretAccessKey: aws.ToString(c.SecretAccessKey),
		SessionToken: aws.ToString(c.SessionToken), Expiration: aws.ToTime(c.Expiration),
	}, nil
}
