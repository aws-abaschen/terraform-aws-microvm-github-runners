// Package awsx adapts AWS services to the runner interfaces.
package awsx

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/runner"
)

// Retention of finished records in the state table. Audit history lives in
// CloudWatch Logs; the table is operational state only.
const (
	claimTTL  = 30 * 24 * time.Hour
	vmTTL     = 30 * 24 * time.Hour
	activeTTL = 2 * 24 * time.Hour // safety net if a record is never ended
)

// Store implements runner.Store on DynamoDB.
type Store struct {
	DB    *dynamodb.Client
	Table string
}

var _ runner.Store = (*Store)(nil)

func s(v string) ddb.AttributeValue { return &ddb.AttributeValueMemberS{Value: v} }
func n(v int64) ddb.AttributeValue {
	return &ddb.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
func jobKey(id int64) map[string]ddb.AttributeValue {
	return map[string]ddb.AttributeValue{"pk": s("job#" + strconv.FormatInt(id, 10))}
}
func vmKey(name string) map[string]ddb.AttributeValue {
	return map[string]ddb.AttributeValue{"pk": s("vm#" + name)}
}

func isConditionFailed(err error) bool {
	var c *ddb.ConditionalCheckFailedException
	return errors.As(err, &c)
}

// Claim implements runner.Store.
func (st *Store) Claim(ctx context.Context, jobID int64, now, staleBefore time.Time) (int, bool, error) {
	stale := int64(0)
	if !staleBefore.IsZero() {
		stale = staleBefore.Unix()
	}
	out, err := st.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &st.Table,
		Key:                      jobKey(jobID),
		UpdateExpression:         aws.String("SET attempt = if_not_exists(attempt, :zero) + :one, #st = :claimed, claimed_at = :now, #ttl = :ttl"),
		ConditionExpression:      aws.String("attribute_not_exists(pk) OR #st = :released OR claimed_at < :stale"),
		ExpressionAttributeNames: map[string]string{"#st": "status", "#ttl": "ttl"},
		ExpressionAttributeValues: map[string]ddb.AttributeValue{
			":zero": n(0), ":one": n(1), ":claimed": s("claimed"), ":released": s("released"),
			":now": n(now.Unix()), ":stale": n(stale), ":ttl": n(now.Add(claimTTL).Unix()),
		},
		ReturnValues: ddb.ReturnValueUpdatedNew,
	})
	if isConditionFailed(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, ok := out.Attributes["attempt"].(*ddb.AttributeValueMemberN)
	if !ok {
		return 0, false, errors.New("claim: missing attempt")
	}
	a, err := strconv.Atoi(v.Value)
	return a, err == nil, err
}

// ReleaseClaim implements runner.Store.
func (st *Store) ReleaseClaim(ctx context.Context, jobID int64) error {
	_, err := st.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &st.Table,
		Key:                       jobKey(jobID),
		UpdateExpression:          aws.String("SET #st = :released"),
		ExpressionAttributeNames:  map[string]string{"#st": "status"},
		ExpressionAttributeValues: map[string]ddb.AttributeValue{":released": s("released")},
	})
	return err
}

// PutVM implements runner.Store.
func (st *Store) PutVM(ctx context.Context, vm runner.VM) error {
	item := vmKey(vm.RunnerName)
	item["runner_name"] = s(vm.RunnerName)
	item["runner_id"] = n(vm.RunnerID)
	item["microvm_id"] = s(vm.MicrovmID)
	item["job_id"] = n(vm.JobID)
	item["status"] = s(vm.Status)
	item["started_at"] = n(vm.StartedAt.Unix())
	item["active"] = s("1")
	item["ttl"] = n(vm.StartedAt.Add(activeTTL).Unix())
	_, err := st.DB.PutItem(ctx, &dynamodb.PutItemInput{TableName: &st.Table, Item: item})
	return err
}

// GetVM implements runner.Store.
func (st *Store) GetVM(ctx context.Context, name string) (*runner.VM, error) {
	out, err := st.DB.GetItem(ctx, &dynamodb.GetItemInput{TableName: &st.Table, Key: vmKey(name), ConsistentRead: aws.Bool(true)})
	if err != nil || out.Item == nil {
		return nil, err
	}
	vm := decodeVM(out.Item)
	return &vm, nil
}

// EndVM implements runner.Store.
func (st *Store) EndVM(ctx context.Context, name, status string, now time.Time) error {
	_, err := st.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &st.Table,
		Key:                      vmKey(name),
		UpdateExpression:         aws.String("SET #st = :s, ended_at = :now, #ttl = :ttl REMOVE active"),
		ConditionExpression:      aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames: map[string]string{"#st": "status", "#ttl": "ttl"},
		ExpressionAttributeValues: map[string]ddb.AttributeValue{
			":s": s(status), ":now": n(now.Unix()), ":ttl": n(now.Add(vmTTL).Unix()),
		},
	})
	if isConditionFailed(err) {
		return nil
	}
	return err
}

// ActiveVMs implements runner.Store using the sparse "active" index.
func (st *Store) ActiveVMs(ctx context.Context) ([]runner.VM, error) {
	p := dynamodb.NewQueryPaginator(st.DB, &dynamodb.QueryInput{
		TableName:                 &st.Table,
		IndexName:                 aws.String("active-index"),
		KeyConditionExpression:    aws.String("active = :one"),
		ExpressionAttributeValues: map[string]ddb.AttributeValue{":one": s("1")},
	})
	var vms []runner.VM
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			vms = append(vms, decodeVM(it))
		}
	}
	return vms, nil
}

func decodeVM(it map[string]ddb.AttributeValue) runner.VM {
	str := func(k string) string {
		if v, ok := it[k].(*ddb.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	num := func(k string) int64 {
		if v, ok := it[k].(*ddb.AttributeValueMemberN); ok {
			i, _ := strconv.ParseInt(v.Value, 10, 64)
			return i
		}
		return 0
	}
	return runner.VM{
		RunnerName: str("runner_name"),
		RunnerID:   num("runner_id"),
		MicrovmID:  str("microvm_id"),
		JobID:      num("job_id"),
		Status:     str("status"),
		StartedAt:  time.Unix(num("started_at"), 0),
	}
}
