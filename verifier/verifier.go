// Package verifier is what every t10 service uses to check a bearer token. It
// verifies offline against the JWKS, and returns the user and any agent context
// together, so a service can't read sub and hand an agent the user's full rights
// by accident. A delegated token fails closed if its grant chain can't be fetched.
package verifier

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	audienceT10 = "t10"
	// chainCacheTTL matches the 60 seconds modules may cache org reads, and is
	// how long a revoked grant can go unnoticed on top of a live token.
	chainCacheTTL  = 60 * time.Second
	jwksCacheTTL   = 10 * time.Minute
	jwksMinRefresh = 30 * time.Second
	requestTimeout = 5 * time.Second
	renewBefore    = 30 * time.Second
)

// ErrInvalidToken means the caller should answer 401.
var ErrInvalidToken = errors.New("invalid token")

// ErrGrantUnavailable means a delegated token's chain could not be confirmed.
// The caller must refuse the request.
var ErrGrantUnavailable = errors.New("grant chain unavailable")

// Config configures New.
type Config struct {
	// Issuer is the auth service's public URL, as it appears in iss.
	Issuer string
	// URL is where this service reaches it, which inside a deployment is often
	// a private address. It defaults to Issuer.
	URL string
	// Service is this service's own name, as it appears in an audience.
	Service string
	// ClientSecret is this service's secret, for fetching grant chains.
	ClientSecret string
	// HTTPClient defaults to one with a short timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time
}

// Link is one grant in a chain. The service must pass every link's constraints.
type Link struct {
	ID          string          `json:"id"`
	Actions     []string        `json:"actions"`
	Audiences   []string        `json:"audiences"`
	Constraints json.RawMessage `json:"constraints"`
	ExpiresAt   time.Time       `json:"expires_at"`
}

// Agent is set when an agent is acting for the user.
type Agent struct {
	// Runtime is the service account running the agent.
	Runtime string
	GrantID string
	// Chain runs from the root grant to this one.
	Chain []Link
}

// Identity is who a verified token speaks for.
type Identity struct {
	// UserID is empty on a service's own token.
	UserID   string
	TenantID string
	// ServiceID is set only on a service's own token, which has no user.
	ServiceID string
	Scopes    []string
	// Agent is nil for a person calling directly.
	Agent *Agent
}

// Allows reports whether the token may attempt action. A person's rights are
// the service's to judge, so it's always true for them. An agent needs action in
// its token and on every grant in its chain, on top of what the user may do.
func (i *Identity) Allows(action string) bool {
	if i.Agent == nil {
		return i.UserID != ""
	}
	if !slices.Contains(i.Scopes, action) {
		return false
	}
	return !slices.ContainsFunc(i.Agent.Chain, func(l Link) bool { return !slices.Contains(l.Actions, action) })
}

// Constraints returns every grant's opaque constraints, root first. All apply.
func (i *Identity) Constraints() []json.RawMessage {
	if i.Agent == nil {
		return nil
	}
	out := make([]json.RawMessage, 0, len(i.Agent.Chain))
	for _, link := range i.Agent.Chain {
		out = append(out, link.Constraints)
	}
	return out
}

type claims struct {
	jwt.RegisteredClaims
	TenantID string `json:"tid"`
	Scope    string `json:"scope"`
	Session  string `json:"sid"`
	Actor    *struct {
		Subject string `json:"sub"`
	} `json:"act"`
	Grant string `json:"grant"`
}

type cachedChain struct {
	chain   []Link
	fetched time.Time
}

// Verifier checks tokens. It's safe for concurrent use.
type Verifier struct {
	cfg    Config
	client *http.Client
	now    func() time.Time

	mu           sync.Mutex
	keys         map[string]*ecdsa.PublicKey
	keysFetched  time.Time
	serviceToken string
	serviceUntil time.Time
	chains       map[string]cachedChain
}

// New builds a Verifier.
func New(cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" || cfg.Service == "" {
		return nil, errors.New("verifier: Issuer and Service are required")
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if cfg.URL == "" {
		cfg.URL = cfg.Issuer
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Verifier{cfg: cfg, client: client, now: now, chains: map[string]cachedChain{}}, nil
}

// Verify checks a bearer token and returns who it speaks for. Errors wrap
// ErrInvalidToken or ErrGrantUnavailable, and either means refuse.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Identity, error) {
	parsed := &claims{}
	_, err := jwt.ParseWithClaims(raw, parsed, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	identity := &Identity{TenantID: parsed.TenantID, Scopes: strings.Fields(parsed.Scope)}
	delegated := parsed.Grant != "" || parsed.Actor != nil
	switch {
	case delegated:
		if parsed.Grant == "" || parsed.Actor == nil || !slices.Contains(parsed.Audience, v.cfg.Service) {
			return nil, fmt.Errorf("%w: delegated token is not for this service", ErrInvalidToken)
		}
		chain, err := v.chain(ctx, parsed.Grant)
		if err != nil {
			return nil, err
		}
		identity.UserID = parsed.Subject
		identity.Agent = &Agent{Runtime: parsed.Actor.Subject, GrantID: parsed.Grant, Chain: chain}
	case !slices.Contains(parsed.Audience, audienceT10):
		return nil, fmt.Errorf("%w: audience not accepted", ErrInvalidToken)
	case parsed.Session != "":
		identity.UserID = parsed.Subject
	default:
		identity.ServiceID = parsed.Subject
	}
	return identity, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	age := v.now().Sub(v.keysFetched)
	key, known := v.keys[kid]
	if age > jwksCacheTTL || (!known && age >= jwksMinRefresh) {
		// A failed fetch keeps the old keys: tokens signed by them stay good
		// through an auth outage, and an unknown kid is refused below.
		if keys, err := v.fetchKeys(ctx); err == nil {
			v.keys = keys
		}
		v.keysFetched = v.now()
		key, known = v.keys[kid]
	}
	if !known {
		return nil, errors.New("unknown kid")
	}
	return key, nil
}

func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.URL+"/.well-known/jwks.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	keys := map[string]*ecdsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" {
			continue
		}
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
			continue
		}
		// This also refuses a point that isn't on the curve.
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			continue
		}
		keys[k.Kid] = key
	}
	return keys, nil
}

func (v *Verifier) chain(ctx context.Context, grantID string) ([]Link, error) {
	v.mu.Lock()
	cached, ok := v.chains[grantID]
	v.mu.Unlock()
	if ok && v.now().Sub(cached.fetched) < chainCacheTTL {
		return cached.chain, nil
	}
	chain, err := v.fetchChain(ctx, grantID)
	if err != nil {
		v.mu.Lock()
		delete(v.chains, grantID)
		v.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrGrantUnavailable, err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, c := range v.chains {
		if v.now().Sub(c.fetched) >= chainCacheTTL {
			delete(v.chains, id)
		}
	}
	v.chains[grantID] = cachedChain{chain: chain, fetched: v.now()}
	return chain, nil
}

func (v *Verifier) fetchChain(ctx context.Context, grantID string) ([]Link, error) {
	token, err := v.ownToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.URL+"/v1/grants/"+grantID+"/chain", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chain status %d", resp.StatusCode)
	}
	var doc struct {
		Chain []Link `json:"chain"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Chain) == 0 {
		return nil, errors.New("empty chain")
	}
	return doc.Chain, nil
}

func (v *Verifier) ownToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.serviceUntil.Sub(v.now()) > renewBefore {
		return v.serviceToken, nil
	}
	body, err := json.Marshal(map[string]string{
		"grant_type": "client_credentials", "client_id": v.cfg.Service, "client_secret": v.cfg.ClientSecret,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cfg.URL+"/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("service token status %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	v.serviceToken = result.AccessToken
	v.serviceUntil = v.now().Add(time.Duration(result.ExpiresIn) * time.Second)
	return v.serviceToken, nil
}
