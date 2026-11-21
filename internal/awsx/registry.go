package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/broker"
)

// Registry implements broker.Registry with one SSM parameter per repository,
// <Prefix>/<repo-id>, holding {"repository":"owner/repo",...}. Its size and
// the broker's configuration stay the same whatever the number of repositories.
type Registry struct {
	Client *ssm.Client
	Prefix string // for example /gha-microvm/registry
}

var _ broker.Registry = (*Registry)(nil)

// Onboarded implements broker.Registry.
func (r *Registry) Onboarded(ctx context.Context, repo string) (bool, error) {
	name := strings.TrimSuffix(r.Prefix, "/") + "/" + broker.RepoID(repo)
	decrypt := true
	out, err := r.Client.GetParameter(ctx, &ssm.GetParameterInput{Name: &name, WithDecryption: &decrypt})
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var entry struct {
		Repository string `json:"repository"`
	}
	if out.Parameter == nil || out.Parameter.Value == nil || json.Unmarshal([]byte(*out.Parameter.Value), &entry) != nil {
		return false, errors.New("unreadable registry entry " + name)
	}
	// The id is a 10-character hash prefix: the full name guards against collisions.
	return strings.EqualFold(entry.Repository, repo), nil
}
