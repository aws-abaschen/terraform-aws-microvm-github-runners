// Package github is a minimal GitHub REST client for a GitHub App.
// It uses only the standard library to keep the supply chain small.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when GitHub answers 404.
var ErrNotFound = errors.New("github: not found")

// App authenticates as a GitHub App and calls the REST API.
type App struct {
	ID      int64
	Key     *rsa.PrivateKey
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	value   string
	expires time.Time
}

// Credentials is the JSON document stored in Secrets Manager.
type Credentials struct {
	AppID      int64  `json:"app_id"`
	PrivateKey string `json:"private_key"`
}

// NewApp parses the credentials secret.
func NewApp(secret []byte, baseURL string, client *http.Client) (*App, error) {
	var c Credentials
	if err := json.Unmarshal(secret, &c); err != nil {
		return nil, fmt.Errorf("parse app credentials: %w", err)
	}
	if c.AppID == 0 || c.PrivateKey == "" {
		return nil, errors.New("app credentials need app_id and private_key")
	}
	key, err := parseKey(c.PrivateKey)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &App{ID: c.AppID, Key: key, BaseURL: strings.TrimRight(baseURL, "/"), HTTP: client, Now: time.Now, tokens: map[string]cachedToken{}}, nil
}

func parseKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("private_key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rk, nil
}

// JWT returns a short-lived app JWT (RS256).
func (a *App) JWT() (string, error) {
	now := a.Now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// RepoToken returns an installation token scoped to one repository.
func (a *App) RepoToken(ctx context.Context, owner, repo string) (string, error) {
	cacheKey := owner + "/" + repo
	a.mu.Lock()
	if t, ok := a.tokens[cacheKey]; ok && a.Now().Before(t.expires.Add(-5*time.Minute)) {
		a.mu.Unlock()
		return t.value, nil
	}
	a.mu.Unlock()

	jwt, err := a.JWT()
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.do(ctx, "Bearer "+jwt, http.MethodGet, fmt.Sprintf("/repos/%s/%s/installation", owner, repo), nil, &inst); err != nil {
		return "", fmt.Errorf("get installation: %w", err)
	}
	// Scope the token to the single repository and the permissions we need.
	body := map[string]any{
		"repositories": []string{repo},
		"permissions":  map[string]string{"administration": "write", "actions": "read"},
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.do(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), body, &tok); err != nil {
		return "", fmt.Errorf("create installation token: %w", err)
	}
	a.mu.Lock()
	a.tokens[cacheKey] = cachedToken{value: tok.Token, expires: tok.ExpiresAt}
	a.mu.Unlock()
	return tok.Token, nil
}

// JITConfig is the answer of generate-jitconfig.
type JITConfig struct {
	Encoded  string
	RunnerID int64
}

// GenerateJITConfig registers a single-use runner on a repository.
func (a *App) GenerateJITConfig(ctx context.Context, token, owner, repo, name string, groupID int64, labels []string) (JITConfig, error) {
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Encoded string `json:"encoded_jit_config"`
	}
	body := map[string]any{"name": name, "runner_group_id": groupID, "labels": labels, "work_folder": "_work"}
	err := a.do(ctx, "Bearer "+token, http.MethodPost, fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig", owner, repo), body, &out)
	return JITConfig{Encoded: out.Encoded, RunnerID: out.Runner.ID}, err
}

// DeleteRunner removes a runner registration. 404 is treated as success.
func (a *App) DeleteRunner(ctx context.Context, token, owner, repo string, id int64) error {
	err := a.do(ctx, "Bearer "+token, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, repo, id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// RunnerStatus reports whether a runner registration still exists and is busy.
// A JIT runner is removed by GitHub once its single job is done.
func (a *App) RunnerStatus(ctx context.Context, token, owner, repo string, id int64) (exists, busy bool, err error) {
	var r struct {
		ID   int64 `json:"id"`
		Busy bool  `json:"busy"`
	}
	err = a.do(ctx, "Bearer "+token, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, repo, id), nil, &r)
	if errors.Is(err, ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, r.Busy, nil
}

// Job is a workflow job.
type Job struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	Status    string    `json:"status"`
	Labels    []string  `json:"labels"`
	CreatedAt time.Time `json:"created_at"`
}

// QueuedJobs lists queued jobs of queued workflow runs (bounded to maxRuns runs).
func (a *App) QueuedJobs(ctx context.Context, token, owner, repo string, maxRuns int) ([]Job, error) {
	var runs struct {
		Runs []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?status=queued&per_page=%d", owner, repo, maxRuns)
	if err := a.do(ctx, "Bearer "+token, http.MethodGet, path, nil, &runs); err != nil {
		return nil, err
	}
	var jobs []Job
	for _, r := range runs.Runs {
		var page struct {
			Jobs []Job `json:"jobs"`
		}
		p := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100", owner, repo, r.ID)
		if err := a.do(ctx, "Bearer "+token, http.MethodGet, p, nil, &page); err != nil {
			return nil, err
		}
		for _, j := range page.Jobs {
			if j.Status == "queued" {
				jobs = append(jobs, j)
			}
		}
	}
	return jobs, nil
}

// APIError carries the HTTP status of a failed call.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

func (a *App) do(ctx context.Context, auth, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "microvm-github-runners")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// NewClient returns an API client without an App key. Use it with an
// external token source (the token broker).
func NewClient(baseURL string) *App {
	return &App{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}, Now: time.Now, tokens: map[string]cachedToken{}}
}
