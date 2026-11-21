package broker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

type registry map[string]bool

func (r registry) Onboarded(_ context.Context, repo string) (bool, error) { return r[repo], nil }

type tokens struct{ calls int }

func (t *tokens) RepoToken(context.Context, string, string) (string, error) {
	t.calls++
	return "ghs_scoped", nil
}

// stsTransport answers like STS for whatever caller ARN the test sets.
type stsTransport struct {
	arn, account string
	hits         int
}

func (s *stsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.hits++
	body := `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>` + s.arn + `</Arn><Account>` + s.account + `</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

const goodURL = "https://sts.eu-west-1.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15&X-Amz-Signature=abc"

func newBroker(arn, account string) (*Broker, *tokens, *stsTransport) {
	tr := &stsTransport{arn: arn, account: account}
	tk := &tokens{}
	return &Broker{
		Prefix: "gha-microvm", Account: "111111111111", Region: "eu-west-1", Partition: "aws",
		Registry: registry{"acme/app": true, "acme/other": true},
		Tokens:   tk, HTTP: &http.Client{Transport: tr},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
	}, tk, tr
}

func roleARN(repo string) string {
	return "arn:aws:sts::111111111111:assumed-role/gha-microvm-" + RepoID(repo) + "-control/dispatcher"
}

func TestIssuesTokenToOwningRole(t *testing.T) {
	b, tk, _ := newBroker(roleARN("acme/app"), "111111111111")
	res, err := b.Handle(context.Background(), Request{Repository: "ACME/app", CallerIdentityURL: goodURL})
	if err != nil || res.Token != "ghs_scoped" || tk.calls != 1 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestDenials(t *testing.T) {
	cases := []struct {
		name, arn, account, repo, url string
		stsCalled                     bool
	}{
		{"other repo's role", roleARN("acme/other"), "111111111111", "acme/app", goodURL, true},
		{"foreign account", roleARN("acme/app"), "222222222222", "acme/app", goodURL, true},
		{"not onboarded", roleARN("acme/new"), "111111111111", "acme/new", goodURL, false},
		{"ssrf host", roleARN("acme/app"), "111111111111", "acme/app", "https://evil.example.com/?Action=GetCallerIdentity&X-Amz-Signature=a", false},
		{"other region", roleARN("acme/app"), "111111111111", "acme/app", "https://sts.us-east-1.amazonaws.com/?Action=GetCallerIdentity&X-Amz-Signature=a", false},
		{"plain http", roleARN("acme/app"), "111111111111", "acme/app", "http://sts.eu-west-1.amazonaws.com/?Action=GetCallerIdentity&X-Amz-Signature=a", false},
		{"other action", roleARN("acme/app"), "111111111111", "acme/app", "https://sts.eu-west-1.amazonaws.com/?Action=AssumeRole&X-Amz-Signature=a", false},
		{"unsigned", roleARN("acme/app"), "111111111111", "acme/app", "https://sts.eu-west-1.amazonaws.com/?Action=GetCallerIdentity", false},
		{"bad repo", roleARN("acme/app"), "111111111111", "acme", goodURL, false},
		{"prefix trick", "arn:aws:sts::111111111111:assumed-role/gha-microvm-" + RepoID("acme/app") + "-control-evil/x", "111111111111", "acme/app", goodURL, true},
	}
	for _, c := range cases {
		b, tk, tr := newBroker(c.arn, c.account)
		_, err := b.Handle(context.Background(), Request{Repository: c.repo, CallerIdentityURL: c.url})
		if err == nil || tk.calls != 0 {
			t.Errorf("%s: expected denial", c.name)
		}
		if (tr.hits > 0) != c.stsCalled {
			t.Errorf("%s: sts called=%v", c.name, tr.hits > 0)
		}
	}
}

func TestRepoIDMatchesTerraform(t *testing.T) {
	// terraform console: substr(sha256(lower("Acme/App")), 0, 10)
	if got := RepoID("Acme/App"); got != RepoID("acme/app") || len(got) != 10 {
		t.Fatalf("got %s", got)
	}
}
