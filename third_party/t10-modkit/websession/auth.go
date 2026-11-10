package websession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors the auth service answers with. Each ends a different way for the user.
var (
	// ErrSignedOut means the session can't be refreshed, so they sign in again.
	ErrSignedOut = errors.New("signed out")
	// ErrTenantRequired means this user belongs to several tenants, and the
	// code flow can't say which one they want.
	ErrTenantRequired = errors.New("tenant required")
	// ErrTenantDenied means they aren't an active member of that tenant.
	ErrTenantDenied = errors.New("tenant denied")
	// ErrMFARequired means the tenant demands a second factor.
	ErrMFARequired = errors.New("second factor required")
	// ErrUnavailable means the auth service gave no usable answer.
	ErrUnavailable = errors.New("auth service unavailable")
)

// Tokens is a token response.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
}

// AuthClient calls the auth service as this app's own confidential client.
type AuthClient struct {
	URL, ClientID, ClientSecret string
	// HTTP needs a timeout, because a refresh holds a row lock across this call.
	HTTP *http.Client
	// Inject, if set, adds the caller's trace to each request, so the auth service's
	// spans join the app's.
	Inject func(context.Context, http.Header)
}

// Redeem exchanges a code. A refused redemption leaves the code unspent.
func (a *AuthClient) Redeem(ctx context.Context, code, redirectURI, verifier, tenant string) (Tokens, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier},
	}
	if tenant != "" {
		form.Set("tenant", tenant)
	}
	return a.token(ctx, form)
}

// Refresh spends the refresh credential and returns its replacement. Passing a
// tenant switches the session's active tenant.
func (a *AuthClient) Refresh(ctx context.Context, refresh, tenant string) (Tokens, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	if tenant != "" {
		form.Set("tenant", tenant)
	}
	return a.token(ctx, form)
}

// Revoke ends the client session at the auth service.
func (a *AuthClient) Revoke(ctx context.Context, refresh string) error {
	form := url.Values{"refresh_token": {refresh}}
	resp, err := a.postForm(ctx, "/v1/revoke", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: revoke answered %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}

// Call makes a user-token request and returns the status and body. A network
// failure is ErrUnavailable.
func (a *AuthClient) Call(ctx context.Context, method, path, bearer string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if a.Inject != nil {
		a.Inject(ctx, req.Header)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return resp.StatusCode, data, nil
}

func (a *AuthClient) token(ctx context.Context, form url.Values) (Tokens, error) {
	resp, err := a.postForm(ctx, "/v1/token", form)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Tokens{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, tokenError(resp.StatusCode, data)
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.AccessToken == "" {
		return Tokens{}, fmt.Errorf("%w: malformed token response", ErrUnavailable)
	}
	return Tokens{
		Access: body.AccessToken, Refresh: body.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
	}, nil
}

func tokenError(status int, data []byte) error {
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(data, &body)
	switch {
	case status == http.StatusForbidden && body.Error == "mfa_required":
		return ErrMFARequired
	case status == http.StatusForbidden:
		return ErrTenantDenied
	case body.Error == "invalid_grant":
		return ErrSignedOut
	// The only invalid_request a well-formed call can get, and it has no code
	// of its own.
	case body.Error == "invalid_request" && strings.Contains(body.Description, "tenant"):
		return ErrTenantRequired
	default:
		return fmt.Errorf("%w: token endpoint answered %d %s", ErrUnavailable, status, body.Error)
	}
}

func (a *AuthClient) postForm(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	form.Set("client_id", a.ClientID)
	form.Set("client_secret", a.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if a.Inject != nil {
		a.Inject(ctx, req.Header)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return resp, nil
}
