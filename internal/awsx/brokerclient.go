package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/broker"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/github"
)

// BrokeredGitHub is the GitHub client used by repository control functions.
// It never sees the App key: tokens come from the platform token broker.
type BrokeredGitHub struct {
	*github.App
	Lambda   *lambda.Client
	Presign  *sts.PresignClient
	Function string

	mu      sync.Mutex
	token   string
	expires time.Time
}

// RepoToken asks the broker for a token scoped to owner/repo.
func (b *BrokeredGitHub) RepoToken(ctx context.Context, owner, repo string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	pre, err := b.Presign.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("presign identity: %w", err)
	}
	payload, _ := json.Marshal(broker.Request{Repository: owner + "/" + repo, CallerIdentityURL: pre.URL})
	out, err := b.Lambda.Invoke(ctx, &lambda.InvokeInput{FunctionName: &b.Function, Payload: payload})
	if err != nil {
		return "", fmt.Errorf("invoke broker: %w", err)
	}
	if out.FunctionError != nil {
		return "", errors.New("broker refused: " + string(out.Payload))
	}
	var res broker.Response
	if err := json.Unmarshal(out.Payload, &res); err != nil || res.Token == "" {
		return "", errors.New("broker returned no token")
	}
	b.token, b.expires = res.Token, res.ExpiresAt
	return res.Token, nil
}
