package workitems

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/xiant10/t10-modkit/ids"
	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
)

const (
	// maxLinks is the most links one item holds.
	maxLinks     = 20
	maxLinkURL   = 2000
	maxLinkTitle = 200
)

// LinkKinds are what a link can point at.
var LinkKinds = []string{"pull_request", "commit", "build", "document", "other"}

// ErrTooManyLinks means the item holds as many links as it can.
var ErrTooManyLinks = errors.New("too many links")

// Link points from an item to where its work happens, such as a pull request.
// It's data: nothing here fetches it.
type Link struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	AddedBy   string    `json:"added_by"`
	CreatedAt time.Time `json:"created_at"`
}

// NewLink is a link to add.
type NewLink struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// linksColumn reads an item's links as one JSON array, oldest first.
const linksColumn = `COALESCE((SELECT jsonb_agg(jsonb_build_object('id', l.id, 'url', l.url, 'title', l.title, 'kind', l.kind,
		'added_by', l.added_by, 'created_at', l.created_at) ORDER BY l.created_at, l.id)
	FROM collab.work_item_links l WHERE l.tenant_id = i.tenant_id AND l.item_id = i.id), '[]'::jsonb)`

func scanLinks(data []byte) ([]Link, error) {
	links := []Link{}
	if len(data) == 0 {
		return links, nil
	}
	return links, json.Unmarshal(data, &links)
}

// check cleans a new link: an https URL with a host and no credentials, a
// title, the URL's host and path when none is given, and a known kind, guessed
// from the URL when none is given.
func (n NewLink) check() (NewLink, error) {
	n.URL, n.Title = strings.TrimSpace(n.URL), strings.TrimSpace(n.Title)
	u, err := url.Parse(n.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(n.URL) > maxLinkURL {
		return n, ErrInvalid
	}
	if n.Kind == "" {
		n.Kind = guessKind(u.Path)
	}
	if n.Title == "" {
		n.Title = shortTitle(u.Host + strings.TrimRight(u.Path, "/"))
	}
	if utf8.RuneCountInString(n.Title) > maxLinkTitle || !slices.Contains(LinkKinds, n.Kind) {
		return n, ErrInvalid
	}
	return n, nil
}

// kindsByPath guesses a link's kind from the common code hosts' and CI
// services' paths, in order.
var kindsByPath = []struct{ part, kind string }{
	{"/pull/", "pull_request"}, {"/pulls/", "pull_request"}, {"/merge_requests/", "pull_request"}, {"/pull-requests/", "pull_request"},
	{"/commit/", "commit"}, {"/commits/", "commit"},
	{"/actions/runs/", "build"}, {"/pipelines/", "build"}, {"/builds/", "build"}, {"/jobs/", "build"},
}

func guessKind(path string) string {
	for _, k := range kindsByPath {
		if strings.Contains(path+"/", k.part) {
			return k.kind
		}
	}
	return "other"
}

// shortTitle keeps at most maxLinkTitle characters, ending in an ellipsis
// when it cuts.
func shortTitle(s string) string {
	if utf8.RuneCountInString(s) <= maxLinkTitle {
		return s
	}
	return string([]rune(s)[:maxLinkTitle-1]) + "…"
}

// AddLink adds a link to an item, for anyone who may edit it. The people on the
// item are told, as for an edit.
func (s *Service) AddLink(ctx context.Context, c policy.Caller, id string, n NewLink) (Link, error) {
	n, err := n.check()
	if err != nil {
		return Link{}, err
	}
	link := Link{ID: ids.New("lnk"), URL: n.URL, Title: n.Title, Kind: n.Kind, AddedBy: c.UserID}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.CanEdit() {
			return policy.ErrForbidden
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM collab.work_item_links WHERE tenant_id = $1 AND item_id = $2",
			c.TenantID, id).Scan(&count); err != nil {
			return fmt.Errorf("count links: %w", err)
		}
		if count >= maxLinks {
			return ErrTooManyLinks
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO collab.work_item_links (tenant_id, id, item_id, url, title, kind, added_by) VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING created_at`, c.TenantID, link.ID, id, link.URL, link.Title, link.Kind, c.UserID).Scan(&link.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert link: %w", err)
		}
		return recordLink(ctx, tx, c, id, "link.added", link)
	})
	if err != nil {
		return Link{}, err
	}
	return link, nil
}

// RemoveLink removes a link from an item, for anyone who may edit it.
func (s *Service) RemoveLink(ctx context.Context, c policy.Caller, id, linkID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, relation, err := Lock(ctx, tx, c, id)
		if err != nil {
			return err
		}
		if !relation.CanEdit() {
			return policy.ErrForbidden
		}
		var link Link
		err = tx.QueryRow(ctx, `DELETE FROM collab.work_item_links WHERE tenant_id = $1 AND item_id = $2 AND id = $3
			RETURNING id, url, title, kind`, c.TenantID, id, linkID).Scan(&link.ID, &link.URL, &link.Title, &link.Kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return policy.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete link: %w", err)
		}
		return recordLink(ctx, tx, c, id, "link.removed", link)
	})
}

func recordLink(ctx context.Context, tx pgx.Tx, c policy.Caller, id, kind string, link Link) error {
	notify, err := activity.PeopleOn(ctx, tx, c.TenantID, id)
	if err != nil {
		return err
	}
	return activity.Record(ctx, tx, c, activity.Change{
		ItemID: id, Kind: kind, Detail: map[string]any{"link_id": link.ID, "url": link.URL, "title": link.Title, "kind": link.Kind}, Notify: notify,
	})
}
