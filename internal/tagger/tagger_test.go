package tagger

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	issuer = "https://token.actions.githubusercontent.com"
	aud    = "gha-microvm-tagger"
)

var now = time.Unix(1_800_000_000, 0)

type keyPair struct {
	kid  string
	priv *rsa.PrivateKey
}

func newKey(t *testing.T, kid string) keyPair {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{kid, k}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func sign(t *testing.T, k keyPair, header, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.priv, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func claims(mod map[string]any) map[string]any {
	c := map[string]any{
		"iss": issuer, "aud": aud, "exp": now.Add(5 * time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
		"iat": now.Add(-time.Minute).Unix(), "jti": "j-1", "repository": "Acme/App", "repository_owner": "Acme",
		"environment": "runners", "run_id": "42", "run_attempt": "1", "sub": "repo:Acme/App:environment:runners",
		"workflow_ref": "Acme/App/.github/workflows/deploy-runners.yml@refs/heads/main",
	}
	for k, v := range mod {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func jwks(t *testing.T, keys ...keyPair) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/.well-known/jwks" {
			http.NotFound(w, r)
			return
		}
		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		for _, k := range keys {
			set.Keys = append(set.Keys, map[string]string{
				"kty": "RSA", "use": "sig", "kid": k.kid, "alg": "RS256",
				"n": b64(k.priv.N.Bytes()), "e": b64(big.NewInt(int64(k.priv.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

type registry map[string]Entry

func (r registry) Lookup(_ context.Context, repo string) (Entry, bool, error) {
	e, ok := r[repo]
	return e, ok, nil
}

type replay map[string]bool

func (r replay) Claim(_ context.Context, jti string, _ time.Time) (bool, error) {
	if r[jti] {
		return false, nil
	}
	r[jti] = true
	return true, nil
}

type assumer struct{ got []AssumeInput }

func (a *assumer) Assume(_ context.Context, in AssumeInput) (Response, error) {
	a.got = append(a.got, in)
	return Response{AccessKeyID: "ASIA", SecretAccessKey: "s", SessionToken: "t", Expiration: now.Add(time.Hour)}, nil
}

type fixture struct {
	t      *Tagger
	key    keyPair
	sts    *assumer
	verify *Verifier
}

func newFixture(t *testing.T) fixture {
	key := newKey(t, "k1")
	srv, _ := jwks(t, key)
	// The test server stands in for the issuer's JWKS; claims still name GitHub.
	v := &Verifier{Issuer: issuer, Audience: aud, HTTP: redirect(srv), Now: func() time.Time { return now }}
	a := &assumer{}
	return fixture{
		t: &Tagger{
			Verifier: v, Owners: []string{"acme"},
			Registry: registry{"acme/app": {Repository: "acme/app", Environment: "runners"}, "acme/prod": {Repository: "acme/prod", Environment: "production"}},
			Replay:   replay{}, STS: a, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		key: key, sts: a, verify: v,
	}
}

// redirect sends every request to srv, keeping the path.
func redirect(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(r2)
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rs256(kid string) map[string]any {
	return map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
}

func TestIssuesTaggedSession(t *testing.T) {
	f := newFixture(t)
	res, err := f.t.Handle(context.Background(), Request{Token: sign(t, f.key, rs256("k1"), claims(nil))})
	if err != nil || res.AccessKeyID != "ASIA" || len(f.sts.got) != 1 {
		t.Fatalf("res %+v err %v", res, err)
	}
	in := f.sts.got[0]
	if in.Tags["gha-repo"] != "acme/app" || len(in.Tags["gha-repo-id"]) != 10 || in.Duration != time.Hour {
		t.Fatalf("tags %+v", in)
	}
	if in.SourceIdentity != "acme+app@42" || !strings.HasSuffix(in.SessionName, "-42-1") {
		t.Fatalf("identity %q session %q", in.SourceIdentity, in.SessionName)
	}
}

func TestDenies(t *testing.T) {
	other := newKey(t, "k1") // same kid, different key
	cases := []struct {
		name   string
		key    func(f fixture) keyPair
		header map[string]any
		claims map[string]any
	}{
		{"alg none", nil, map[string]any{"alg": "none", "kid": "k1"}, nil},
		{"alg HS256", nil, map[string]any{"alg": "HS256", "kid": "k1"}, nil},
		{"unknown kid", nil, rs256("k9"), nil},
		{"wrong key", func(fixture) keyPair { return other }, rs256("k1"), nil},
		{"expired", nil, nil, map[string]any{"exp": now.Add(-2 * time.Minute).Unix()}},
		{"no exp", nil, nil, map[string]any{"exp": nil}},
		{"not yet valid", nil, nil, map[string]any{"nbf": now.Add(5 * time.Minute).Unix()}},
		{"issued in future", nil, nil, map[string]any{"iat": now.Add(5 * time.Minute).Unix()}},
		{"wrong audience", nil, nil, map[string]any{"aud": "sts.amazonaws.com"}},
		{"two audiences", nil, nil, map[string]any{"aud": []string{aud, "sts.amazonaws.com"}}},
		{"wrong issuer", nil, nil, map[string]any{"iss": "https://evil.example"}},
		{"no jti", nil, nil, map[string]any{"jti": nil}},
		{"owner not allowed", nil, nil, map[string]any{"repository": "evil/app", "repository_owner": "evil"}},
		{"owner mismatch", nil, nil, map[string]any{"repository_owner": "evil"}},
		{"not onboarded", nil, nil, map[string]any{"repository": "acme/new"}},
		{"no environment", nil, nil, map[string]any{"environment": nil}},
		{"environment mismatch", nil, nil, map[string]any{"repository": "acme/prod", "environment": "runners"}},
		{"bad repository", nil, nil, map[string]any{"repository": "acme"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			k := f.key
			if tc.key != nil {
				k = tc.key(f)
			}
			h := tc.header
			if h == nil {
				h = rs256("k1")
			}
			_, err := f.t.Handle(context.Background(), Request{Token: sign(t, k, h, claims(tc.claims))})
			if !errors.Is(err, ErrDenied) || len(f.sts.got) != 0 {
				t.Fatalf("err %v, assumed %d", err, len(f.sts.got))
			}
		})
	}
}

func TestDeniesReplay(t *testing.T) {
	f := newFixture(t)
	tok := sign(t, f.key, rs256("k1"), claims(nil))
	if _, err := f.t.Handle(context.Background(), Request{Token: tok}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.t.Handle(context.Background(), Request{Token: tok}); !errors.Is(err, ErrDenied) {
		t.Fatalf("replay err %v", err)
	}
}

func TestDeniesGarbage(t *testing.T) {
	f := newFixture(t)
	for _, tok := range []string{"", "a.b", "a.b.c", "!!.!!.!!"} {
		if _, err := f.t.Handle(context.Background(), Request{Token: tok}); !errors.Is(err, ErrDenied) {
			t.Fatalf("%q: %v", tok, err)
		}
	}
}

func TestJWKSRefreshOnRotation(t *testing.T) {
	k1, k2 := newKey(t, "k1"), newKey(t, "k2")
	srv, hits := jwks(t, k1, k2)
	clock := now
	v := &Verifier{Issuer: issuer, Audience: aud, HTTP: redirect(srv), Now: func() time.Time { return clock }}
	v.keys, v.fetched = nil, time.Time{}
	if _, err := v.Verify(context.Background(), sign(t, k1, rs256("k1"), claims(nil))); err != nil {
		t.Fatal(err)
	}
	// Unknown kid within a minute of the last fetch: no new fetch.
	if _, err := v.Verify(context.Background(), sign(t, k1, rs256("k3"), claims(nil))); !errors.Is(err, ErrToken) || *hits != 1 {
		t.Fatalf("err %v hits %d", err, *hits)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := v.Verify(context.Background(), sign(t, k2, rs256("k2"), claims(nil))); err != nil || *hits != 1 {
		t.Fatalf("cached k2: err %v hits %d", err, *hits)
	}
	if _, err := v.Verify(context.Background(), sign(t, k1, rs256("k3"), claims(nil))); !errors.Is(err, ErrToken) || *hits != 2 {
		t.Fatalf("refetch: err %v hits %d", err, *hits)
	}
}

func TestVerifierUnavailableIsNotDenied(t *testing.T) {
	f := newFixture(t)
	f.verify.HTTP = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })}
	_, err := f.t.Handle(context.Background(), Request{Token: sign(t, f.key, rs256("k1"), claims(nil))})
	if err == nil || errors.Is(err, ErrDenied) {
		t.Fatalf("err %v", err)
	}
}
