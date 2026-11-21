package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/broker"
	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/tagger"
)

// Registry implements broker.Registry and tagger.Registry with one SSM
// parameter per repository, <Prefix>/<repo-id>, holding
// {"repository":"owner/repo","environment":"runners"}. Its size and the
// functions' configuration stay the same whatever the number of repositories.
type Registry struct {
	Client *ssm.Client
	Prefix string // for example /gha-microvm/registry
}

var (
	_ broker.Registry = (*Registry)(nil)
	_ tagger.Registry = (*Registry)(nil)
)

// Lookup implements tagger.Registry.
func (r *Registry) Lookup(ctx context.Context, repo string) (tagger.Entry, bool, error) {
	var entry tagger.Entry
	name := strings.TrimSuffix(r.Prefix, "/") + "/" + broker.RepoID(repo)
	decrypt := true
	out, err := r.Client.GetParameter(ctx, &ssm.GetParameterInput{Name: &name, WithDecryption: &decrypt})
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return entry, false, nil
	}
	if err != nil {
		return entry, false, err
	}
	if out.Parameter == nil || out.Parameter.Value == nil || json.Unmarshal([]byte(*out.Parameter.Value), &entry) != nil {
		return entry, false, errors.New("unreadable registry entry " + name)
	}
	// The id is a 10-character hash prefix: the full name guards against collisions.
	return entry, strings.EqualFold(entry.Repository, repo), nil
}

// Onboarded implements broker.Registry.
func (r *Registry) Onboarded(ctx context.Context, repo string) (bool, error) {
	_, ok, err := r.Lookup(ctx, repo)
	return ok, err
}
