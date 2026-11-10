// Package websession is the sign-in every t10 web app shares: the cookie, the code
// flow, token refresh, tenant switching, step-up and one sign-out for every app. The
// browser holds a random cookie and never a t10 token; the tokens live sealed in the
// app's own table. An app gives its cookie name and table, so two apps' sessions
// never mix.
package websession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// renewBefore replaces an access token this long before it expires, so a
	// request that has just left can't arrive with a dead one.
	renewBefore = 60 * time.Second
	// defaultMaxAge matches a client's absolute_ttl in clients.yaml when the app sets none.
	defaultMaxAge = 7 * 24 * time.Hour
	// loginLimit bounds how long a sign-in can take, including a QR approval.
	loginLimit = 10 * time.Minute

	purposeLogin   = "login"
	purposeAccess  = "access"
	purposeRefresh = "refresh"
)

// Options configures a Manager.
type Options struct {
	Pool          *pgxpool.Pool
	Key           []byte
	Auth          *AuthClient
	AuthPublicURL string
	PublicURL     string
	// Secure is false only on a plain-http local address.
	Secure bool
	// Name starts this app's cookie names, such as "wb" for wb_session and wb_login.
	Name string
	// Table holds this app's sessions, schema-qualified, such as "workbench.web_sessions".
	Table string
	// MaxAge is how long a session lasts at most, matching the client's absolute_ttl.
	MaxAge time.Duration
	// ReplaceOnSignIn ends the session a new sign-in comes from, as a step-up needs.
	ReplaceOnSignIn bool
}

// Manager keeps one row per signed-in browser.
type Manager struct {
	opts   Options
	sealer *sealer
	cookie string
	login  string
}

// NewManager builds a Manager.
func NewManager(opts Options) (*Manager, error) {
	s, err := newSealer(opts.Key)
	if err != nil {
		return nil, err
	}
	if opts.Name == "" || !validTable(opts.Table) {
		return nil, fmt.Errorf("websession: a cookie name and a schema.table are required, not %q and %q", opts.Name, opts.Table)
	}
	if opts.MaxAge == 0 {
		opts.MaxAge = defaultMaxAge
	}
	cookie := opts.Name + "_session"
	if opts.Secure {
		cookie = "__Host-" + cookie
	}
	return &Manager{opts: opts, sealer: s, cookie: cookie, login: opts.Name + "_login"}, nil
}

// validTable is a schema-qualified name of lower-case letters, digits and
// underscores: the only thing put into a query's text.
func validTable(name string) bool {
	schema, table, ok := strings.Cut(name, ".")
	plain := func(part string) bool {
		return part != "" && strings.IndexFunc(part, func(r rune) bool {
			return r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) < 0
	}
	return ok && plain(schema) && plain(table)
}

// sql puts this app's table into a query.
func (m *Manager) sql(query string) string {
	return strings.ReplaceAll(query, "{sessions}", m.opts.Table)
}

// Identity is who a request acts for, with a token good for at least a minute.
type Identity struct {
	UserID string
	Access string
}

// login is the sign-in in progress, held in a sealed cookie until the callback.
type login struct {
	State    string    `json:"state"`
	Verifier string    `json:"verifier"`
	Next     string    `json:"next"`
	Expires  time.Time `json:"expires"`
}

// Begin stores a new sign-in and returns the address to send the browser to.
func (m *Manager) Begin(w http.ResponseWriter, next, tenant string) string {
	l := login{
		State: randomToken(), Verifier: randomToken(), Next: SafeNext(next),
		Expires: time.Now().Add(loginLimit),
	}
	encoded, _ := json.Marshal(l)
	http.SetCookie(w, &http.Cookie{
		Name: m.login, Value: base64.RawURLEncoding.EncodeToString(m.sealer.seal(encoded, purposeLogin)),
		Path: "/auth", HttpOnly: true, Secure: m.opts.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(loginLimit.Seconds()),
	})
	challenge := sha256.Sum256([]byte(l.Verifier))
	query := url.Values{
		"response_type": {"code"}, "client_id": {m.opts.Auth.ClientID}, "redirect_uri": {m.redirectURI()},
		"state": {l.State}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	if tenant != "" {
		query.Set("tenant", tenant)
	}
	return m.opts.AuthPublicURL + "/v1/authorize?" + query.Encode()
}

// BeginStepUp is Begin through the sign-in page first, because the code it then
// redeems has to carry a fresh second factor. Use it with ReplaceOnSignIn.
func (m *Manager) BeginStepUp(w http.ResponseWriter, next string) string {
	authorize := strings.TrimPrefix(m.Begin(w, next, ""), m.opts.AuthPublicURL)
	return m.opts.AuthPublicURL + "/v1/signin?" + url.Values{"reason": {"stepup"}, "next": {authorize}}.Encode()
}

// Complete finishes the code flow, sets the session cookie, and returns the
// path to go back to.
func (m *Manager) Complete(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, error) {
	l, err := m.readLogin(r)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: m.login, Path: "/auth", MaxAge: -1, HttpOnly: true, Secure: m.opts.Secure, SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if q.Get("state") != l.State {
		return "", errors.New("state does not match this sign-in")
	}
	if denied := q.Get("error"); denied != "" {
		return "", fmt.Errorf("sign-in refused: %s", denied)
	}
	tokens, err := m.opts.Auth.Redeem(ctx, q.Get("code"), m.redirectURI(), l.Verifier, "")
	if err != nil {
		return "", err
	}

	cookie := randomToken()
	sub, err := subject(tokens.Access)
	if err != nil {
		return "", err
	}
	_, err = m.opts.Pool.Exec(ctx, m.sql(`
		INSERT INTO {sessions} (id, user_id, refresh_token, access_token, access_expires_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`),
		hashID(cookie), sub, m.sealer.seal([]byte(tokens.Refresh), purposeRefresh),
		m.sealer.seal([]byte(tokens.Access), purposeAccess), tokens.ExpiresAt, time.Now().Add(m.opts.MaxAge))
	if err != nil {
		return "", fmt.Errorf("store session: %w", err)
	}
	// A step-up replaces the session it came from. A failure there leaves a client
	// session that runs out on its own, so it doesn't stop this sign-in.
	if m.opts.ReplaceOnSignIn {
		if err := m.end(ctx, r); err != nil {
			slog.Warn("previous session not ended", "error", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: m.cookie, Value: cookie, Path: "/", HttpOnly: true, Secure: m.opts.Secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(m.opts.MaxAge.Seconds()),
	})
	return l.Next, nil
}

// Identify returns the request's identity, refreshing its token if needed.
func (m *Manager) Identify(ctx context.Context, r *http.Request) (Identity, error) {
	id, ok := m.sessionID(r)
	if !ok {
		return Identity{}, ErrSignedOut
	}
	var userID string
	var sealed []byte
	var expires time.Time
	err := m.opts.Pool.QueryRow(ctx, m.sql(`
		SELECT user_id, access_token, access_expires_at FROM {sessions}
		WHERE id = $1 AND expires_at > now()`), id).Scan(&userID, &sealed, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrSignedOut
	}
	if err != nil {
		return Identity{}, fmt.Errorf("read session: %w", err)
	}
	if time.Until(expires) > renewBefore {
		access, err := m.sealer.open(sealed, purposeAccess)
		if err != nil {
			return Identity{}, fmt.Errorf("open access token: %w", err)
		}
		return Identity{UserID: userID, Access: string(access)}, nil
	}
	return m.refresh(ctx, id, "")
}

// SwitchTenant moves the session to tenant. Its old access token stops being
// used at once, so nothing can act in the old tenant from here on.
func (m *Manager) SwitchTenant(ctx context.Context, r *http.Request, tenant string) error {
	id, ok := m.sessionID(r)
	if !ok {
		return ErrSignedOut
	}
	_, err := m.refresh(ctx, id, tenant)
	return err
}

// refresh runs under a row lock, so two replicas can't spend the same rotating
// secret. A waiter finds the new token and spends nothing, unless it is
// switching tenant.
func (m *Manager) refresh(ctx context.Context, id, tenant string) (Identity, error) {
	var result Identity
	var dropped bool
	err := pgx.BeginFunc(ctx, m.opts.Pool, func(tx pgx.Tx) error {
		var sealedRefresh, sealedAccess []byte
		var expires time.Time
		err := tx.QueryRow(ctx, m.sql(`
			SELECT user_id, refresh_token, access_token, access_expires_at FROM {sessions}
			WHERE id = $1 AND expires_at > now() FOR UPDATE`), id).
			Scan(&result.UserID, &sealedRefresh, &sealedAccess, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSignedOut
		}
		if err != nil {
			return fmt.Errorf("lock session: %w", err)
		}
		if tenant == "" && time.Until(expires) > renewBefore {
			access, err := m.sealer.open(sealedAccess, purposeAccess)
			if err != nil {
				return fmt.Errorf("open access token: %w", err)
			}
			result.Access = string(access)
			return nil
		}
		refresh, err := m.sealer.open(sealedRefresh, purposeRefresh)
		if err != nil {
			return fmt.Errorf("open refresh token: %w", err)
		}
		tokens, err := m.opts.Auth.Refresh(ctx, string(refresh), tenant)
		if errors.Is(err, ErrSignedOut) {
			dropped = true
			return nil
		}
		if err != nil {
			return err
		}
		result.Access = tokens.Access
		_, err = tx.Exec(ctx, m.sql(`
			UPDATE {sessions} SET refresh_token = $2, access_token = $3, access_expires_at = $4 WHERE id = $1`),
			id, m.sealer.seal([]byte(tokens.Refresh), purposeRefresh), m.sealer.seal([]byte(tokens.Access), purposeAccess), tokens.ExpiresAt)
		return err
	})
	if err != nil {
		return Identity{}, err
	}
	if dropped {
		if _, err := m.opts.Pool.Exec(ctx, m.sql("DELETE FROM {sessions} WHERE id = $1"), id); err != nil {
			return Identity{}, fmt.Errorf("drop session: %w", err)
		}
		return Identity{}, ErrSignedOut
	}
	return result, nil
}

// Logout ends the session here and at the auth service. A failure there still
// signs the browser out, and the client session runs out on its own.
func (m *Manager) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	http.SetCookie(w, &http.Cookie{Name: m.cookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.opts.Secure, SameSite: http.SameSiteLaxMode})
	return m.end(ctx, r)
}

// end deletes the request's session and revokes its refresh token.
func (m *Manager) end(ctx context.Context, r *http.Request) error {
	id, ok := m.sessionID(r)
	if !ok {
		return nil
	}
	var sealed []byte
	err := m.opts.Pool.QueryRow(ctx, m.sql("DELETE FROM {sessions} WHERE id = $1 RETURNING refresh_token"), id).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	refresh, err := m.sealer.open(sealed, purposeRefresh)
	if err != nil {
		return fmt.Errorf("open refresh token: %w", err)
	}
	return m.opts.Auth.Revoke(ctx, string(refresh))
}

// Prune deletes sessions past their absolute limit.
func (m *Manager) Prune(ctx context.Context) error {
	_, err := m.opts.Pool.Exec(ctx, m.sql("DELETE FROM {sessions} WHERE expires_at <= now()"))
	return err
}

func (m *Manager) sessionID(r *http.Request) (string, bool) {
	c, err := r.Cookie(m.cookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	return hashID(c.Value), true
}

func (m *Manager) readLogin(r *http.Request) (login, error) {
	c, err := r.Cookie(m.login)
	if err != nil {
		return login{}, errors.New("no sign-in in progress")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return login{}, fmt.Errorf("read sign-in cookie: %w", err)
	}
	plain, err := m.sealer.open(sealed, purposeLogin)
	if err != nil {
		return login{}, fmt.Errorf("open sign-in cookie: %w", err)
	}
	var l login
	if err := json.Unmarshal(plain, &l); err != nil {
		return login{}, fmt.Errorf("parse sign-in cookie: %w", err)
	}
	if time.Now().After(l.Expires) {
		return login{}, errors.New("sign-in took too long")
	}
	return l, nil
}

// OnPublicHost sends a sign-in that started on another name for this app, such as
// localhost for 127.0.0.1 or www for the bare domain, to the public name first. The
// sign-in's cookie is kept per host name, and sign-in returns to the public one, so it
// would never finish. Cookies ignore ports, so a dev proxy on another port stays. It
// reports whether it redirected.
func (m *Manager) OnPublicHost(w http.ResponseWriter, r *http.Request) bool {
	public, err := url.Parse(m.opts.PublicURL)
	if err != nil || public.Hostname() == "" || strings.EqualFold(public.Hostname(), (&url.URL{Host: r.Host}).Hostname()) {
		return false
	}
	http.Redirect(w, r, strings.TrimSuffix(m.opts.PublicURL, "/")+r.URL.RequestURI(), http.StatusFound)
	return true
}

func (m *Manager) redirectURI() string { return m.opts.PublicURL + "/auth/callback" }

// Key identifies a request's session for a short-lived cache. It's a hash, so a
// cache holds nothing that can act as anyone.
func (m *Manager) Key(r *http.Request) string {
	id, _ := m.sessionID(r)
	return id
}

// IsSecure reports whether cookies can demand https for an address.
func IsSecure(publicURL string) bool { return strings.HasPrefix(publicURL, "https://") }

// SafeNext keeps a return path on this site. Anything else goes home.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

// subject reads sub from a token the auth service just handed this service, so
// its signature needs no check here. Every other service verifies it.
func subject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode access token: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("access token has no subject")
	}
	return claims.Sub, nil
}

func hashID(cookie string) string {
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	data := make([]byte, 32)
	_, _ = rand.Read(data)
	return base64.RawURLEncoding.EncodeToString(data)
}
