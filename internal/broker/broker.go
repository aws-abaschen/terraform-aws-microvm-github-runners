// Package broker is the platform-owned token broker. Only the broker can
// read the GitHub App private key. A repository's control role proves who it
// is with a presigned STS GetCallerIdentity request and receives an
// installation token scoped to its own repository only.
package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request is sent by a repository's control role.
type Request struct {
	Repository        string `json:"repository"`
	CallerIdentityURL string `json:"caller_identity_url"`
}

// Response carries a repository-scoped installation token.
type Response struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RepoID is the stable short id of a repository used in resource names.
// It must match the Terraform expression substr(sha256(lower(repo)), 0, 10).
func RepoID(repo string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repo)))
	return hex.EncodeToString(sum[:])[:10]
}

// Registry tells whether a repository is onboarded in this account.
type Registry interface {
	Onboarded(ctx context.Context, repo string) (bool, error)
}

// TokenSource mints repository-scoped installation tokens.
type TokenSource interface {
	RepoToken(ctx context.Context, owner, repo string) (string, error)
}

// Broker validates callers and vends tokens.
type Broker struct {
	Prefix    string // resource name prefix, e.g. gha-microvm
	Account   string
	Region    string
	Partition string
	Registry  Registry
	Tokens    TokenSource
	HTTP      *http.Client
	Log       *slog.Logger
	Now       func() time.Time
}

// ErrDenied is returned for any authorization failure. Details go to the log only.
var ErrDenied = errors.New("denied")

// Handle serves one request.
func (b *Broker) Handle(ctx context.Context, req Request) (Response, error) {
	repo := strings.ToLower(strings.TrimSpace(req.Repository))
	log := b.Log.With("repository", repo)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		log.Warn("audit: broker denied", "audit", true, "event", "broker.denied", "reason", "bad repository")
		return Response{}, ErrDenied
	}
	onboarded, err := b.Registry.Onboarded(ctx, repo)
	if err != nil {
		log.Error("registry", "err", err)
		return Response{}, errors.New("registry unavailable")
	}
	if !onboarded {
		log.Warn("audit: broker denied", "audit", true, "event", "broker.denied", "reason", "repository not onboarded")
		return Response{}, ErrDenied
	}
	arn, err := b.callerARN(ctx, req.CallerIdentityURL)
	if err != nil {
		log.Warn("audit: broker denied", "audit", true, "event", "broker.denied", "reason", err.Error())
		return Response{}, ErrDenied
	}
	want := fmt.Sprintf("arn:%s:sts::%s:assumed-role/%s-%s-control/", b.Partition, b.Account, b.Prefix, RepoID(repo))
	if !strings.HasPrefix(arn, want) {
		log.Warn("audit: broker denied", "audit", true, "event", "broker.denied", "reason", "caller role does not own repository", "caller", arn)
		return Response{}, ErrDenied
	}
	tok, err := b.Tokens.RepoToken(ctx, owner, name)
	if err != nil {
		log.Error("mint token", "err", err)
		return Response{}, errors.New("token unavailable")
	}
	log.Info("audit: token issued", "audit", true, "event", "broker.issued", "caller", arn)
	// Installation tokens live one hour; advertise less so callers refresh early.
	return Response{Token: tok, ExpiresAt: b.Now().Add(45 * time.Minute)}, nil
}

// callerARN executes a presigned sts:GetCallerIdentity after checking it
// really targets this region's STS endpoint (no SSRF to other hosts).
func (b *Broker) callerARN(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("bad url")
	}
	host := "sts." + b.Region + ".amazonaws.com"
	if b.Partition == "aws-cn" {
		host += ".cn"
	}
	q := u.Query()
	if u.Scheme != "https" || u.Host != host || u.User != nil || (u.Path != "" && u.Path != "/") ||
		q.Get("Action") != "GetCallerIdentity" || q.Get("X-Amz-Signature") == "" {
		return "", errors.New("not a presigned GetCallerIdentity for this region")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := b.HTTP.Do(r)
	if err != nil {
		return "", fmt.Errorf("sts: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sts status %d", resp.StatusCode)
	}
	var out struct {
		Result struct {
			Arn     string `xml:"Arn"`
			Account string `xml:"Account"`
		} `xml:"GetCallerIdentityResult"`
	}
	if err := xml.Unmarshal(body, &out); err != nil || out.Result.Arn == "" {
		return "", errors.New("sts: unreadable response")
	}
	if out.Result.Account != b.Account {
		return "", errors.New("caller from another account")
	}
	return out.Result.Arn, nil
}
