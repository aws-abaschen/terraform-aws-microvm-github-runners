// Package tagger turns a GitHub Actions OIDC token into a session of the
// shared deploy role tagged with the repository (gha-repo-id, gha-repo).
// GitHub tokens can't carry AWS session tags, so this platform function sets
// them after checking the token, the owner, the registry entry and replay.
// Design: docs/iam.html (phase 2).
package tagger

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws-abaschen/terraform-aws-microvm-github-runners/internal/broker"
)

// Request is sent by the deploy workflow through the entry role.
type Request struct {
	Token string `json:"token"` // GitHub OIDC token with the tagger audience
}

// Response holds the tagged deploy role credentials, in the shape of
// `aws sts assume-role` so workflows can export them as is.
type Response struct {
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	SessionToken    string    `json:"SessionToken"`
	Expiration      time.Time `json:"Expiration"`
}

// Entry is a repository's registry record.
type Entry struct {
	Repository  string `json:"repository"`
	Environment string `json:"environment"`
}

// Registry finds the onboarding record of a repository.
type Registry interface {
	Lookup(ctx context.Context, repo string) (Entry, bool, error)
}

// Replay records token ids; Claim is false when the id was already used.
type Replay interface {
	Claim(ctx context.Context, jti string, until time.Time) (bool, error)
}

// AssumeInput describes the tagged session.
type AssumeInput struct {
	SessionName    string
	SourceIdentity string
	Tags           map[string]string
	Duration       time.Duration
}

// Assumer starts the shared deploy role session.
type Assumer interface {
	Assume(ctx context.Context, in AssumeInput) (Response, error)
}

// TokenVerifier checks a token and returns its claims.
type TokenVerifier interface {
	Verify(ctx context.Context, raw string) (Claims, error)
}

// Tagger serves one request per invocation.
type Tagger struct {
	Verifier TokenVerifier
	Owners   []string // allowed repository owners, lower case
	Registry Registry
	Replay   Replay
	STS      Assumer
	Duration time.Duration // session length; role chaining caps it at 1h
	Log      *slog.Logger
}

// ErrDenied is returned for every rejection; the reason goes to the audit log.
var ErrDenied = errors.New("denied")

var unsafeID = regexp.MustCompile(`[^\w+=,.@-]`)

// Handle validates the token and returns tagged credentials.
func (t *Tagger) Handle(ctx context.Context, req Request) (Response, error) {
	deny := func(log *slog.Logger, reason string, err error) (Response, error) {
		log.Warn("audit: tagger denied", "audit", true, "event", "tagger.denied", "reason", reason, "err", err)
		return Response{}, ErrDenied
	}
	c, err := t.Verifier.Verify(ctx, req.Token)
	if err != nil {
		if !errors.Is(err, ErrToken) {
			t.Log.Error("verify", "err", err)
			return Response{}, errors.New("verifier unavailable")
		}
		return deny(t.Log, "token", err)
	}
	repo := strings.ToLower(c.Repository)
	log := t.Log.With("repository", repo, "run_id", c.RunID, "jti", c.ID, "workflow_ref", c.Workflow)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" || strings.Contains(name, "/") || !strings.EqualFold(owner, c.RepositoryOwner) {
		return deny(log, "bad repository claim", nil)
	}
	if !slices.Contains(t.Owners, owner) {
		return deny(log, "owner not allowed", nil)
	}
	if c.Environment == "" {
		return deny(log, "no environment", nil)
	}
	entry, found, err := t.Registry.Lookup(ctx, repo)
	if err != nil {
		log.Error("registry", "err", err)
		return Response{}, errors.New("registry unavailable")
	}
	if !found || !strings.EqualFold(entry.Repository, repo) {
		return deny(log, "not onboarded", nil)
	}
	if c.Environment != entry.Environment {
		return deny(log, "environment mismatch", nil)
	}
	fresh, err := t.Replay.Claim(ctx, c.ID, time.Unix(c.Expiry, 0))
	if err != nil {
		log.Error("replay", "err", err)
		return Response{}, errors.New("replay store unavailable")
	}
	if !fresh {
		return deny(log, "replayed jti", nil)
	}
	id := broker.RepoID(repo)
	d := t.Duration
	if d == 0 || d > time.Hour {
		d = time.Hour
	}
	in := AssumeInput{
		SessionName:    trim(unsafeID.ReplaceAllString(id+"-"+c.RunID+"-"+c.RunAttempt, "_"), 64),
		SourceIdentity: trim(unsafeID.ReplaceAllString(strings.ReplaceAll(repo, "/", "+")+"@"+c.RunID, "_"), 64),
		Tags:           map[string]string{"gha-repo-id": id, "gha-repo": repo},
		Duration:       d,
	}
	res, err := t.STS.Assume(ctx, in)
	if err != nil {
		log.Error("assume deploy role", "err", err)
		return Response{}, errors.New("assume failed")
	}
	log.Info("audit: tagger issued", "audit", true, "event", "tagger.issued", "repo_id", id,
		"source_identity", in.SourceIdentity, "session", in.SessionName, "expires", res.Expiration)
	return res, nil
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
