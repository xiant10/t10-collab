// Package proposals stores the drafts agents make through t10-modkit/ontology:
// in memory, for tests and test modules, or in Postgres, in the module's own
// schema. The flow itself (the 7-day expiry, the 50-draft limit, confirming
// under the person's current rights) is ontology's, so every store keeps it.
package proposals

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xiant10/t10-modkit/ontology"
	"github.com/xiant10/t10-modkit/pgdb"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations are the store's tables in the module's schema, for pgdb.Migrate
// with the module's own lock key. They are recorded in the schema's
// schema_migrations beside the module's own.
func Migrations(schema string, lock int64) pgdb.Migrations {
	return pgdb.Migrations{FS: inSchema(migrations, schema), Schema: schema, Lock: lock}
}

func inSchema(source embed.FS, schema string) fstest.MapFS {
	out := fstest.MapFS{}
	entries, _ := source.ReadDir("migrations")
	for _, e := range entries {
		body, _ := source.ReadFile("migrations/" + e.Name())
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(string(body), "{{schema}}", schema))}
	}
	return out
}

// Memory keeps drafts in memory.
type Memory struct {
	mu   sync.Mutex
	rows []ontology.Proposal
}

func (m *Memory) CreateProposal(_ context.Context, p ontology.Proposal, limit int, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	open := 0
	for _, r := range m.rows {
		if r.Tenant == p.Tenant && r.User == p.User && r.State == "open" && now.Before(r.ExpiresAt) {
			open++
		}
	}
	if open >= limit {
		return ontology.ErrTooManyProposals
	}
	m.rows = append(m.rows, p)
	return nil
}

func (m *Memory) OpenProposals(_ context.Context, tenant, user string, now time.Time) ([]ontology.Proposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ontology.Proposal{}
	for _, r := range slices.Backward(m.rows) {
		if r.Tenant == tenant && r.User == user && r.State == "open" && now.Before(r.ExpiresAt) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Memory) GetProposal(_ context.Context, tenant, user, id string) (ontology.Proposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.Tenant == tenant && r.User == user && r.ID == id {
			return r, nil
		}
	}
	return ontology.Proposal{}, ontology.ErrNotFound
}

func (m *Memory) DecideProposal(_ context.Context, tenant, user, id, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rows {
		if r.Tenant == tenant && r.User == user && r.ID == id {
			if r.State != from {
				return ontology.ErrProposalClosed
			}
			m.rows[i].State = to
			return nil
		}
	}
	return ontology.ErrNotFound
}

// Age moves every draft's times back, for a test of expiry.
func (m *Memory) Age(by time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		m.rows[i].CreatedAt = m.rows[i].CreatedAt.Add(-by)
		m.rows[i].ExpiresAt = m.rows[i].ExpiresAt.Add(-by)
	}
}

// Postgres keeps drafts in the module's schema, after Migrations ran.
type Postgres struct {
	Pool   *pgxpool.Pool
	Schema string
}

func (s Postgres) table() string { return s.Schema + ".ontology_proposals" }

func (s Postgres) CreateProposal(ctx context.Context, p ontology.Proposal, limit int, now time.Time) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// One person's drafts are counted one request at a time.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, s.table()+"|"+p.Tenant+"|"+p.User); err != nil {
			return err
		}
		var open int
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id=$1 AND user_id=$2 AND state='open' AND expires_at>$3`, s.table()),
			p.Tenant, p.User, now).Scan(&open); err != nil {
			return err
		}
		if open >= limit {
			return ontology.ErrTooManyProposals
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id,id,user_id,agent,action,object,params,state,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,'open',$8,$9)`, s.table()),
			p.Tenant, p.ID, p.User, p.Agent, p.Action, p.Object, string(p.Params), p.CreatedAt, p.ExpiresAt)
		return err
	})
}

const columns = `id,tenant_id,user_id,agent,action,object,params,state,created_at,expires_at`

func scan(row pgx.Row) (ontology.Proposal, error) {
	var p ontology.Proposal
	var params []byte
	err := row.Scan(&p.ID, &p.Tenant, &p.User, &p.Agent, &p.Action, &p.Object, &params, &p.State, &p.CreatedAt, &p.ExpiresAt)
	p.Params = params
	return p, err
}

func (s Postgres) OpenProposals(ctx context.Context, tenant, user string, now time.Time) ([]ontology.Proposal, error) {
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=$1 AND user_id=$2 AND state='open' AND expires_at>$3 ORDER BY created_at DESC, id DESC LIMIT 100`, columns, s.table()), tenant, user, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ontology.Proposal{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s Postgres) GetProposal(ctx context.Context, tenant, user, id string) (ontology.Proposal, error) {
	p, err := scan(s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id=$1 AND user_id=$2 AND id=$3`, columns, s.table()), tenant, user, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ontology.Proposal{}, ontology.ErrNotFound
	}
	return p, err
}

func (s Postgres) DecideProposal(ctx context.Context, tenant, user, id, from, to string) error {
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET state=$5, decided_at=CASE WHEN $5='open' THEN NULL ELSE clock_timestamp() END WHERE tenant_id=$1 AND user_id=$2 AND id=$3 AND state=$4`, s.table()), tenant, user, id, from, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetProposal(ctx, tenant, user, id); err != nil {
			return err
		}
		return ontology.ErrProposalClosed
	}
	return nil
}
