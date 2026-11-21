package tagger

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Claims are the GitHub Actions OIDC claims the tagger uses. Only values from
// a verified token are trusted; sub is ignored because organisations can
// customise its format.
type Claims struct {
	Issuer          string   `json:"iss"`
	Audience        audience `json:"aud"`
	Expiry          int64    `json:"exp"`
	NotBefore       int64    `json:"nbf"`
	IssuedAt        int64    `json:"iat"`
	ID              string   `json:"jti"`
	Repository      string   `json:"repository"`
	RepositoryOwner string   `json:"repository_owner"`
	Environment     string   `json:"environment"`
	RunID           string   `json:"run_id"`
	RunAttempt      string   `json:"run_attempt"`
	Workflow        string   `json:"workflow_ref"`
}

// audience accepts the string and the array form of aud.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Verifier checks GitHub Actions OIDC tokens: RS256 only, a key from the
// issuer's JWKS, exact issuer and audience, and the time claims.
type Verifier struct {
	Issuer   string // https://token.actions.githubusercontent.com
	Audience string // gha-microvm-tagger
	HTTP     *http.Client
	Now      func() time.Time
	Skew     time.Duration // allowed clock difference, default 60s
	CacheTTL time.Duration // JWKS cache, default 1h

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// ErrToken is wrapped by every token rejection.
var ErrToken = errors.New("invalid token")

func tokenErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrToken, fmt.Sprintf(format, a...))
}

// Verify returns the claims of a valid token.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	var c Claims
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return c, tokenErr("not a JWS")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return c, tokenErr("header encoding")
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return c, tokenErr("header json")
	}
	if h.Alg != "RS256" { // no none, no HS256 with the public key
		return c, tokenErr("alg %q", h.Alg)
	}
	if h.Kid == "" {
		return c, tokenErr("no kid")
	}
	key, err := v.key(ctx, h.Kid)
	if err != nil {
		return c, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, tokenErr("signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return c, tokenErr("signature")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, tokenErr("payload encoding")
	}
	if err := json.Unmarshal(pb, &c); err != nil {
		return Claims{}, tokenErr("payload json")
	}
	return c, v.check(c)
}

func (v *Verifier) check(c Claims) error {
	if c.Issuer != v.Issuer {
		return tokenErr("issuer %q", c.Issuer)
	}
	if len(c.Audience) != 1 || c.Audience[0] != v.Audience {
		return tokenErr("audience %q", []string(c.Audience))
	}
	skew := v.Skew
	if skew == 0 {
		skew = time.Minute
	}
	now := v.Now()
	switch {
	case c.Expiry == 0 || !now.Before(time.Unix(c.Expiry, 0).Add(skew)):
		return tokenErr("expired")
	case c.NotBefore != 0 && now.Add(skew).Before(time.Unix(c.NotBefore, 0)):
		return tokenErr("not yet valid")
	case c.IssuedAt == 0 || now.Add(skew).Before(time.Unix(c.IssuedAt, 0)):
		return tokenErr("issued in the future")
	case c.ID == "":
		return tokenErr("no jti")
	}
	return nil
}

// key returns the signing key for kid, refreshing the JWKS when it is stale
// or the kid is unknown (GitHub rotates keys). Unknown kids trigger at most one
// fetch a minute.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	ttl := v.CacheTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	age := v.Now().Sub(v.fetched)
	if k, ok := v.keys[kid]; ok && age < ttl {
		return k, nil
	}
	if v.keys == nil || age >= time.Minute {
		keys, err := v.fetch(ctx)
		if err != nil {
			if k, ok := v.keys[kid]; ok { // keep serving a known key if GitHub is unreachable
				return k, nil
			}
			return nil, fmt.Errorf("jwks: %w", err)
		}
		v.keys, v.fetched = keys, v.Now()
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, tokenErr("unknown kid %q", kid)
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(v.Issuer, "/")+"/.well-known/jwks", nil)
	if err != nil {
		return nil, err
	}
	res, err := v.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable keys")
	}
	return keys, nil
}
