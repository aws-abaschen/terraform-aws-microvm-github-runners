package github

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := `{"action":"queued"}`
	if !VerifySignature([]byte("s3cret"), []byte(body), sign("s3cret", body)) {
		t.Fatal("valid signature rejected")
	}
	for _, h := range []string{"", "sha1=abc", sign("other", body), "sha256=zz"} {
		if VerifySignature([]byte("s3cret"), []byte(body), h) {
			t.Fatalf("invalid signature %q accepted", h)
		}
	}
	if VerifySignature(nil, []byte(body), sign("", body)) {
		t.Fatal("empty secret must never verify")
	}
}

func testApp(t *testing.T, srv *httptest.Server) *App {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	secret, _ := json.Marshal(Credentials{AppID: 99, PrivateKey: string(pemKey)})
	a, err := NewApp(secret, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRepoTokenAndJIT(t *testing.T) {
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" "+r.URL.Path]++
		switch r.URL.Path {
		case "/repos/acme/app/installation":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Errorf("installation lookup must use the app JWT")
			}
			_, _ = w.Write([]byte(`{"id":7}`))
		case "/app/installations/7/access_tokens":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if repos, _ := body["repositories"].([]any); len(repos) != 1 || repos[0] != "app" {
				t.Errorf("token must be scoped to the repo, got %v", body["repositories"])
			}
			_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
		case "/repos/acme/app/actions/runners/generate-jitconfig":
			if r.Header.Get("Authorization") != "Bearer ghs_x" {
				t.Errorf("jit must use the installation token")
			}
			_, _ = w.Write([]byte(`{"runner":{"id":5},"encoded_jit_config":"abc"}`))
		case "/repos/acme/app/actions/runners/5":
			_, _ = w.Write([]byte(`{"id":5,"busy":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := testApp(t, srv)
	ctx := context.Background()
	tok, err := a.RepoToken(ctx, "acme", "app")
	if err != nil || tok != "ghs_x" {
		t.Fatalf("token %q err %v", tok, err)
	}
	if _, err := a.RepoToken(ctx, "acme", "app"); err != nil {
		t.Fatal(err)
	}
	if calls["POST /app/installations/7/access_tokens"] != 1 {
		t.Fatal("token should be cached")
	}
	jit, err := a.GenerateJITConfig(ctx, tok, "acme", "app", "mvm-1-1", 1, []string{"microvm"})
	if err != nil || jit.Encoded != "abc" || jit.RunnerID != 5 {
		t.Fatalf("jit %+v err %v", jit, err)
	}
	exists, busy, err := a.RunnerStatus(ctx, tok, "acme", "app", 5)
	if err != nil || !exists || !busy {
		t.Fatalf("status %v %v %v", exists, busy, err)
	}
	exists, _, err = a.RunnerStatus(ctx, tok, "acme", "app", 6)
	if err != nil || exists {
		t.Fatal("404 must mean runner gone")
	}
	if err := a.DeleteRunner(ctx, tok, "acme", "app", 6); err != nil {
		t.Fatal("deleting a missing runner is fine")
	}
}

func TestNewAppRejectsBadSecrets(t *testing.T) {
	for _, s := range []string{`{}`, `not json`, `{"app_id":1,"private_key":"nope"}`} {
		if _, err := NewApp([]byte(s), "https://api.github.com", nil); err == nil {
			t.Fatalf("%s accepted", s)
		}
	}
}
