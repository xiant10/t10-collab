package websession

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
)

// SignOut ends this app's session whatever else happens, and returns where to send
// the browser next, to end the sign-in and the other apps' sessions behind it.
func (m *Manager) SignOut(ctx context.Context, w http.ResponseWriter, r *http.Request) string {
	target := m.signOutTarget(ctx, r)
	if err := m.Logout(ctx, w, r); err != nil {
		slog.Warn("sign-out incomplete", "error", err)
	}
	return target
}

// signOutTarget is the ticket's address when auth-server gives one. With no session
// left to ask for one, auth-server's own page finishes it. If auth-server isn't
// answering, the app says so instead of claiming more than it did.
func (m *Manager) signOutTarget(ctx context.Context, r *http.Request) string {
	const incomplete = "/signed-out?reason=incomplete"
	hosted := m.opts.AuthPublicURL + "/v1/signout"
	id, err := m.Identify(ctx, r)
	if errors.Is(err, ErrSignedOut) {
		return hosted
	}
	if err != nil {
		return incomplete
	}
	status, body, err := m.opts.Auth.Call(ctx, http.MethodPost, "/v1/me/browser-logout", id.Access, map[string]string{
		"client_id": m.opts.Auth.ClientID, "return_to": m.opts.PublicURL + "/signed-out",
	})
	if err != nil || status >= http.StatusInternalServerError {
		return incomplete
	}
	var result struct {
		Ticket string `json:"ticket"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &result) != nil || result.Ticket == "" {
		return hosted
	}
	return m.opts.AuthPublicURL + "/v1/signout/complete?ticket=" + url.QueryEscape(result.Ticket)
}

// Alive reports whether the sign-in behind this session still stands, because one
// sign-out elsewhere leaves this app's token working until it expires. Only a clear
// no ends the session: an auth service that can't answer signs nobody out.
func (m *Manager) Alive(ctx context.Context, r *http.Request) error {
	id, err := m.Identify(ctx, r)
	if err != nil {
		return err
	}
	status, _, err := m.opts.Auth.Call(ctx, http.MethodGet, "/v1/me/session", id.Access, nil)
	if err != nil || status != http.StatusUnauthorized {
		return nil
	}
	if session, ok := m.sessionID(r); ok {
		if _, err := m.opts.Pool.Exec(ctx, m.sql("DELETE FROM {sessions} WHERE id = $1"), session); err != nil {
			return err
		}
	}
	return ErrSignedOut
}

// SignInHints adds to a sign-in address why it's happening and the language to show,
// each of which auth-server checks. Both are only about what the page says.
func SignInHints(target, reason, lang string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	q := u.Query()
	if reason != "" {
		q.Set("reason", reason)
	}
	if lang != "" {
		q.Set("ui_locales", lang)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
