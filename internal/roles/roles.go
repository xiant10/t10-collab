// Package roles reads who administers collab: a tenant admin, or a member whose
// business role grants work:admin. The auth service holds the roles; collab
// reads them fresh on each admin request and never keeps them.
package roles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/xiant10/t10-modkit/people"
)

// Module is collab's entry in the auth service's permission catalog.
const Module = "collab"

// Admin is the permission that makes a member collab's admin without tenant
// admin rights. Collab's keys sit under its action prefix, work, like its agent
// actions.
const Admin = "work:admin"

// Org reads a member's tenant role.
type Org interface {
	Member(ctx context.Context, tenantID, userID string) (people.Member, error)
}

// Client asks the auth service's entitlements route as collab's own service.
type Client struct {
	url    string
	token  func(context.Context) (string, error)
	org    Org
	client *http.Client
}

type grant struct {
	Permission string `json:"permission"`
}

// New reads entitlements at url with collab's service token. org answers for
// tenant admins when this deployment's catalog doesn't name collab.
func New(url string, token func(context.Context) (string, error), org Org) *Client {
	return &Client{url: url, token: token, org: org, client: &http.Client{Timeout: 10 * time.Second}}
}

// Administers answers whether a person may use collab's admin routes. A failed
// read is never a yes: it answers people.ErrUnavailable.
func (c *Client) Administers(ctx context.Context, tenantID, userID string) (bool, error) {
	token, err := c.token(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %v", people.ErrUnavailable, err)
	}
	q := url.Values{"tenant": {tenantID}, "module": {Module}, "user": {userID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/v1/entitlements?"+q.Encode(), nil)
	if err != nil {
		return false, fmt.Errorf("%w: %v", people.ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", people.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	var body struct {
		Error  string `json:"error"`
		Tenant string `json:"tenant_id"`
		Module string `json:"module"`
		Users  []struct {
			ID          string  `json:"user_id"`
			Active      bool    `json:"active"`
			TenantAdmin bool    `json:"tenant_admin"`
			Grants      []grant `json:"grants"`
		} `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("%w: entitlements: %v", people.ErrUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && body.Error == "unknown_module":
		// This deployment gives collab no business roles, so only tenant admins.
		return TenantAdmin(ctx, c.org, tenantID, userID)
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("%w: entitlements status %d", people.ErrUnavailable, resp.StatusCode)
	}
	if body.Tenant != tenantID || body.Module != Module || len(body.Users) != 1 || body.Users[0].ID != userID {
		return false, fmt.Errorf("%w: entitlements answered for someone else", people.ErrUnavailable)
	}
	u := body.Users[0]
	return u.Active && (u.TenantAdmin || slices.Contains(u.Grants, grant{Admin})), nil
}

// TenantAdmin answers whether a person is an active admin of the tenant.
func TenantAdmin(ctx context.Context, org Org, tenantID, userID string) (bool, error) {
	member, err := org.Member(ctx, tenantID, userID)
	if errors.Is(err, people.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return member.Active() && member.Role == "admin", nil
}
