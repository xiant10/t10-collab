// Package sweep is the 15-minute job. The auth service publishes no change
// events, so this module has to find out for itself. It keeps team-linked lists
// current, and moves work away from people who have left.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"t10-collab/internal/activity"
	"t10-collab/internal/lists"
	"t10-collab/internal/people"
	"t10-collab/internal/policy"
)

const (
	// Interval is how often the job runs.
	Interval = 15 * time.Minute
	// lock is the advisory lock key that keeps it to one replica.
	lock = 7410_0003
)

// Org is the org read this package needs.
type Org interface {
	UnitMembers(ctx context.Context, tenantID, unitID string) ([]string, error)
	Members(ctx context.Context, tenantID string) ([]people.Member, error)
}

// Sweeper runs the job.
type Sweeper struct {
	pool *pgxpool.Pool
	org  Org
}

// New builds a Sweeper.
func New(pool *pgxpool.Pool, org Org) *Sweeper { return &Sweeper{pool: pool, org: org} }

// Run sweeps every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Once(ctx); err != nil {
				slog.Error("sweep failed", "error", err)
			}
		}
	}
}

// Once runs one pass, or does nothing if another replica holds the lock.
func (s *Sweeper) Once(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()
	var held bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lock).Scan(&held); err != nil {
		return fmt.Errorf("take sweep lock: %w", err)
	}
	if !held {
		return nil
	}
	defer func() {
		// The connection returns to the pool, so the lock must not outlive this
		// call even when ctx has already been cancelled.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lock)
	}()
	return errors.Join(s.linkedLists(ctx), s.departures(ctx))
}

type linked struct{ tenantID, id, unitID string }

func (s *Sweeper) linkedLists(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, "SELECT tenant_id, id, org_unit_id FROM collab.lists WHERE org_unit_id IS NOT NULL AND archived_at IS NULL")
	if err != nil {
		return fmt.Errorf("read linked lists: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (linked, error) {
		var l linked
		return l, row.Scan(&l.tenantID, &l.id, &l.unitID)
	})
	if err != nil {
		return fmt.Errorf("read linked lists: %w", err)
	}
	for _, l := range found {
		// One list's failure mustn't stop the rest, and it retries next pass.
		if err := s.syncList(ctx, l); err != nil {
			slog.Warn("sweep skipped a list", "list", l.id, "error", err)
		}
	}
	return ctx.Err()
}

// syncList makes one list match its unit. A failed read changes nothing, and only
// the auth service's own 404 counts as the unit being gone.
func (s *Sweeper) syncList(ctx context.Context, l linked) error {
	members, err := s.org.UnitMembers(ctx, l.tenantID, l.unitID)
	deleted := errors.Is(err, people.ErrNotFound)
	if err != nil && !deleted {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The link may have changed during the read, so the check runs under the lock.
		var current string
		err := tx.QueryRow(ctx,
			"SELECT COALESCE(org_unit_id, '') FROM collab.lists WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE",
			l.tenantID, l.id).Scan(&current)
		if err != nil {
			return fmt.Errorf("lock list: %w", err)
		}
		if current != l.unitID {
			return nil
		}
		c := policy.Caller{TenantID: l.tenantID, UserID: policy.System}
		if deleted {
			return clearLink(ctx, tx, c, l.id)
		}
		if _, err := lists.AddOrgMembers(ctx, tx, l.tenantID, l.id, members); err != nil {
			return err
		}
		_, err = lists.RemoveOrgMembers(ctx, tx, l.tenantID, l.id, members)
		return err
	})
}

// clearLink ends a link to a deleted unit, and tells the list's owners.
func clearLink(ctx context.Context, tx pgx.Tx, c policy.Caller, listID string) error {
	rows, err := tx.Query(ctx, "SELECT user_id FROM collab.list_members WHERE tenant_id = $1 AND list_id = $2 AND role = 'owner'", c.TenantID, listID)
	if err != nil {
		return fmt.Errorf("read list owners: %w", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read list owners: %w", err)
	}
	if err := lists.SetLink(ctx, tx, c, listID, "", nil); err != nil {
		return err
	}
	return activity.Record(ctx, tx, c, activity.Change{
		Kind: "list.unit_deleted", Detail: map[string]any{"list_id": listID}, Notify: owners,
	})
}
