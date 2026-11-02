// Package people reads org facts from the auth service with this service's own
// token. Any failure is an error, never an empty answer, so that callers fail
// closed. See auth-server/docs/service-api.md.
package people

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const (
	// renewBefore replaces a token this long before it expires, so a request
	// that has just left can't arrive with a dead one.
	renewBefore    = 60 * time.Second
	requestTimeout = 5 * time.Second
	// cacheTTL is the longest an org read may be reused.
	cacheTTL = 60 * time.Second
	active   = "active"
	inactive = "inactive"
	// memberPage is the largest page the auth service serves.
	memberPage = 500
)

var (
	// ErrUnavailable means the auth service gave no usable answer.
	ErrUnavailable = errors.New("auth service unavailable")
	// ErrNotFound means the auth service says there's no such member or unit.
	ErrNotFound = errors.New("no such member or unit in this tenant")
)

// idPattern refuses anything that could change an org URL. A service token can
// read any tenant, so a user ID carrying ?tenant=… must never reach a path.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidID reports whether id can name a user or unit.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Options configures New.
type Options struct {
	// URL is the auth service's base URL.
	URL          string
	ClientID     string
	ClientSecret string
	// HTTPClient defaults to one with a short timeout.
	HTTPClient *http.Client
}

// Member is one member's place in the organization.
type Member struct {
	UserID        string `json:"user_id"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	UserStatus    string `json:"user_status"`
	ManagerUserID string `json:"manager_user_id"`
	OrgUnitID     string `json:"org_unit_id"`
	DisplayName   string `json:"display_name"`
}

// Active reports whether the membership and the account are both active.
func (m Member) Active() bool { return m.Status == active && m.UserStatus == active }

// Departed reports whether the auth service says this membership has ended. Only
// this explicit status counts: a member missing from a read has not left.
func (m Member) Departed() bool { return m.Status == inactive }

// Suspended reports whether the membership stands but the account is shut, which
// may be temporary.
func (m Member) Suspended() bool { return m.Status == active && m.UserStatus != active }

// Unit is one org unit.
type Unit struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Client reads the auth service. It's safe for concurrent use.
type Client struct {
	opts   Options
	client *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time

	cacheMu sync.Mutex
	cache   map[string]cachedReports
}

type cachedReports struct {
	userIDs []string
	fetched time.Time
}

// New builds a Client.
func New(opts Options) *Client {
	opts.URL = strings.TrimRight(opts.URL, "/")
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return &Client{opts: opts, client: client, cache: map[string]cachedReports{}}
}

func (c *Client) ownToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Until(c.expires) > renewBefore {
		return c.token, nil
	}
	body, err := json.Marshal(map[string]string{
		"grant_type": "client_credentials", "client_id": c.opts.ClientID, "client_secret": c.opts.ClientSecret,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.URL+"/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := c.client.Do(req)
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
	c.token = result.AccessToken
	c.expires = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	return c.token, nil
}

// get reads an org route for one tenant. The token's tenant is the only one a
// caller can pass here, so a request can't name another.
func (c *Client) get(ctx context.Context, tenantID, path string, query url.Values, out any) error {
	token, err := c.ownToken(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("tenant", tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opts.URL+path+"?"+query.Encode(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

// Member reads a member fresh.
func (c *Client) Member(ctx context.Context, tenantID, userID string) (Member, error) {
	if !ValidID(userID) {
		return Member{}, ErrNotFound
	}
	var m Member
	err := c.get(ctx, tenantID, "/v1/org/members/"+url.PathEscape(userID), nil, &m)
	return m, err
}

// Reports reads everyone beneath a manager, fresh, so that someone who was just
// moved can't be assigned work by their old manager.
func (c *Client) Reports(ctx context.Context, tenantID, userID string) ([]string, error) {
	return c.reports(ctx, tenantID, userID, true)
}

func (c *Client) reports(ctx context.Context, tenantID, userID string, transitive bool) ([]string, error) {
	if !ValidID(userID) {
		return nil, ErrNotFound
	}
	var body struct {
		UserIDs []string `json:"user_ids"`
	}
	err := c.get(ctx, tenantID, "/v1/org/members/"+url.PathEscape(userID)+"/reports", url.Values{"transitive": {strconv.FormatBool(transitive)}}, &body)
	return body.UserIDs, err
}

// CachedReports reads a manager's reports for visibility, which may be up to 60
// seconds old. A failed read returns an error, never an older answer.
func (c *Client) CachedReports(ctx context.Context, tenantID, userID string, transitive bool) ([]string, error) {
	key := tenantID + "/" + userID + "/" + strconv.FormatBool(transitive)
	c.cacheMu.Lock()
	cached, ok := c.cache[key]
	c.cacheMu.Unlock()
	if ok && time.Since(cached.fetched) < cacheTTL {
		return cached.userIDs, nil
	}
	userIDs, err := c.reports(ctx, tenantID, userID, transitive)
	if err != nil {
		return nil, err
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	for k, v := range c.cache {
		if time.Since(v.fetched) >= cacheTTL {
			delete(c.cache, k)
		}
	}
	c.cache[key] = cachedReports{userIDs: userIDs, fetched: time.Now()}
	return userIDs, nil
}

// UnitMembers reads a unit's direct, active members, fresh. ErrNotFound means the
// unit was deleted, and an empty list means it's really empty.
func (c *Client) UnitMembers(ctx context.Context, tenantID, unitID string) ([]string, error) {
	if !ValidID(unitID) {
		return nil, ErrNotFound
	}
	var body struct {
		UserIDs []string `json:"user_ids"`
	}
	err := c.get(ctx, tenantID, "/v1/org/units/"+url.PathEscape(unitID)+"/members", nil, &body)
	return body.UserIDs, err
}

// Members reads every member of a tenant, fresh, including inactive and suspended
// ones. Any failed page fails the read, so a caller never acts on part of a tenant.
func (c *Client) Members(ctx context.Context, tenantID string) ([]Member, error) {
	var all []Member
	cursor := ""
	for {
		var body struct {
			Members    []Member `json:"members"`
			NextCursor string   `json:"next_cursor"`
		}
		query := url.Values{"limit": {strconv.Itoa(memberPage)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		if err := c.get(ctx, tenantID, "/v1/org/members", query, &body); err != nil {
			return nil, err
		}
		all = append(all, body.Members...)
		if body.NextCursor == "" {
			return all, nil
		}
		cursor = body.NextCursor
	}
}

// Search finds active members, by display name and within a unit if given.
func (c *Client) Search(ctx context.Context, tenantID, q, unitID, cursor string, limit int) ([]Member, string, error) {
	var body struct {
		Members    []Member `json:"members"`
		NextCursor string   `json:"next_cursor"`
	}
	query := url.Values{"status": {active}, "limit": {strconv.Itoa(limit)}}
	for key, value := range map[string]string{"q": q, "unit": unitID, "cursor": cursor} {
		if value != "" {
			query.Set(key, value)
		}
	}
	err := c.get(ctx, tenantID, "/v1/org/members", query, &body)
	return body.Members, body.NextCursor, err
}

// Units reads the tenant's org units.
func (c *Client) Units(ctx context.Context, tenantID string) ([]Unit, error) {
	var body struct {
		Units []Unit `json:"units"`
	}
	err := c.get(ctx, tenantID, "/v1/org/units", nil, &body)
	return body.Units, err
}
