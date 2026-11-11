// Package events posts the outbox to t10-automations: one event per agent
// account an item's change concerns, such as work offered to an agent teammate.
// Rows commit with the change, and are posted at least once, so t10-automations
// drops repeats by ID.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	batch    = 100
	interval = 2 * time.Second
	// maxBackoff caps the wait after repeated failures.
	maxBackoff = 5 * time.Minute
	// keepSent is how long a posted row stays, for a look at what was sent.
	keepSent = 7 * 24 * time.Hour
)

// Event is what t10-automations receives. Ref names the record as
// <module>:<type>:<id>, and the payload holds only IDs: an automation's run
// reads the item through its tools, under its own rights.
type Event struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Tenant string `json:"tenant"`
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	// For is the agent account the event concerns.
	For   string `json:"for"`
	Actor string `json:"actor"`
	// CauseGrant is the grant of the run that caused it, when an agent did.
	CauseGrant string `json:"cause_grant,omitempty"`
	// Hops counts agent sends, or agent comments if more, on the item since a
	// person last acted on it.
	Hops int       `json:"hops"`
	At   time.Time `json:"at"`
}

// Poster sends due rows.
type Poster struct {
	pool   *pgxpool.Pool
	url    string
	token  func(context.Context) (string, error)
	client *http.Client
}

// New builds a Poster for t10-automations at url. With no url, Run only prunes,
// so a stack without t10-automations keeps no growing outbox.
func New(pool *pgxpool.Pool, url string, token func(context.Context) (string, error)) *Poster {
	return &Poster{pool: pool, url: url, token: token, client: &http.Client{Timeout: 10 * time.Second}}
}

// Run posts until ctx ends.
func (p *Poster) Run(ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := p.Once(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("post events", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once posts one batch of due rows, and prunes old ones.
func (p *Poster) Once(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, "DELETE FROM collab.outbox WHERE sent_at < now() - make_interval(secs => $1)", keepSent.Seconds()); err != nil {
		return fmt.Errorf("prune outbox: %w", err)
	}
	if p.url == "" {
		_, err := p.pool.Exec(ctx, "UPDATE collab.outbox SET sent_at = now() WHERE sent_at IS NULL")
		return err
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, kind, COALESCE(item_id, ''), for_user_id, actor_user_id, COALESCE(cause_grant_id, ''), hops, created_at
			 FROM collab.outbox WHERE sent_at IS NULL AND next_at <= now()
			 ORDER BY seq LIMIT $1 FOR UPDATE SKIP LOCKED`, batch)
		if err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
			var e Event
			var item string
			err := row.Scan(&e.Tenant, &e.ID, &e.Kind, &item, &e.For, &e.Actor, &e.CauseGrant, &e.Hops, &e.At)
			e.Source = "t10-collab"
			if item != "" {
				e.Ref = "collab:work_item:" + item
			}
			return e, err
		})
		if err != nil || len(due) == 0 {
			return err
		}
		ids := make([]string, len(due))
		for i, e := range due {
			ids[i] = e.Tenant + "/" + e.ID
		}
		if postErr := p.post(ctx, due); postErr != nil {
			_, err := tx.Exec(ctx,
				`UPDATE collab.outbox SET attempts = attempts + 1,
				   next_at = now() + LEAST(make_interval(secs => 2 ^ LEAST(attempts, 12)), make_interval(secs => $2))
				 WHERE tenant_id || '/' || id = ANY($1)`, ids, maxBackoff.Seconds())
			if err != nil {
				return err
			}
			slog.Warn("t10-automations didn't take events; retrying", "count", len(due), "error", postErr)
			return nil
		}
		_, err = tx.Exec(ctx, "UPDATE collab.outbox SET sent_at = now() WHERE tenant_id || '/' || id = ANY($1)", ids)
		return err
	})
}

func (p *Poster) post(ctx context.Context, due []Event) error {
	token, err := p.token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"events": due})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("t10-automations answered %d", resp.StatusCode)
	}
	return nil
}
