// Package undo stores what t10-modkit/ontology needs to undo an applied action
// and to answer an idempotent action's replay: in memory, for tests and test
// modules, or in Postgres, in the module's own schema. The flow (only the
// person it was applied for, only within the action's window, never twice) is
// ontology's, so every store keeps it.
package undo

import (
	"context"
	"embed"
	"errors"
	"fmt"
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
// with the module's own lock key.
func Migrations(schema string, lock int64) pgdb.Migrations {
	out := fstest.MapFS{}
	entries, _ := migrations.ReadDir("migrations")
	for _, e := range entries {
		body, _ := migrations.ReadFile("migrations/" + e.Name())
		out["migrations/"+e.Name()] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(string(body), "{{schema}}", schema))}
	}
	return pgdb.Migrations{FS: out, Schema: schema, Lock: lock}
}

// Memory keeps entries in memory.
type Memory struct {
	mu      sync.Mutex
	entries map[string]ontology.UndoEntry
	replays map[string]ontology.Replay
}

func (m *Memory) init() {
	if m.entries == nil {
		m.entries, m.replays = map[string]ontology.UndoEntry{}, map[string]ontology.Replay{}
	}
}

func (m *Memory) SaveUndo(_ context.Context, u ontology.UndoEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.entries[u.Tenant+"|"+u.ID] = u
	return nil
}

func (m *Memory) GetUndo(_ context.Context, tenant, id string) (ontology.UndoEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	u, ok := m.entries[tenant+"|"+id]
	if !ok {
		return u, ontology.ErrNotFound
	}
	return u, nil
}

func (m *Memory) SetUndone(_ context.Context, tenant, id string, undone bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	u, ok := m.entries[tenant+"|"+id]
	if !ok {
		return ontology.ErrNotFound
	}
	if undone && u.Undone {
		return ontology.ErrUndone
	}
	u.Undone = undone
	m.entries[tenant+"|"+id] = u
	return nil
}

// Age moves every entry's times back, for a test of the window.
func (m *Memory) Age(by time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for k, u := range m.entries {
		u.CreatedAt, u.ExpiresAt = u.CreatedAt.Add(-by), u.ExpiresAt.Add(-by)
		m.entries[k] = u
	}
}

func replayKey(tenant, user, action, key string) string {
	return tenant + "|" + user + "|" + action + "|" + key
}

func (m *Memory) GetReplay(_ context.Context, tenant, user, action, key string) (ontology.Replay, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	r, ok := m.replays[replayKey(tenant, user, action, key)]
	return r, ok, nil
}

func (m *Memory) SaveReplay(_ context.Context, r ontology.Replay) (ontology.Replay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	k := replayKey(r.Tenant, r.User, r.Action, r.Key)
	if kept, ok := m.replays[k]; ok {
		return kept, nil
	}
	m.replays[k] = r
	return r, nil
}

// Postgres keeps entries in the module's schema, after Migrations ran.
type Postgres struct {
	Pool   *pgxpool.Pool
	Schema string
}

func (s Postgres) SaveUndo(ctx context.Context, u ontology.UndoEntry) error {
	payload := string(u.Payload)
	if payload == "" {
		payload = "null"
	}
	_, err := s.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.ontology_undo (tenant_id,id,user_id,action,object,payload,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, s.Schema),
		u.Tenant, u.ID, u.User, u.Action, u.Object, payload, u.CreatedAt, u.ExpiresAt)
	return err
}

func (s Postgres) GetUndo(ctx context.Context, tenant, id string) (ontology.UndoEntry, error) {
	var u ontology.UndoEntry
	var payload []byte
	var undone *time.Time
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT id,tenant_id,user_id,action,object,payload,created_at,expires_at,undone_at FROM %s.ontology_undo WHERE tenant_id=$1 AND id=$2`, s.Schema), tenant, id).
		Scan(&u.ID, &u.Tenant, &u.User, &u.Action, &u.Object, &payload, &u.CreatedAt, &u.ExpiresAt, &undone)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ontology.ErrNotFound
	}
	u.Payload, u.Undone = payload, undone != nil
	return u, err
}

func (s Postgres) SetUndone(ctx context.Context, tenant, id string, undone bool) error {
	query := `UPDATE %s.ontology_undo SET undone_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND undone_at IS NULL`
	if !undone {
		query = `UPDATE %s.ontology_undo SET undone_at=NULL WHERE tenant_id=$1 AND id=$2`
	}
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(query, s.Schema), tenant, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetUndo(ctx, tenant, id); err != nil {
			return err
		}
		return ontology.ErrUndone
	}
	return nil
}

func (s Postgres) GetReplay(ctx context.Context, tenant, user, action, key string) (ontology.Replay, bool, error) {
	r := ontology.Replay{Tenant: tenant, User: user, Action: action, Key: key}
	var answer []byte
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT hash,answer FROM %s.ontology_replays WHERE tenant_id=$1 AND user_id=$2 AND action=$3 AND key=$4`, s.Schema), tenant, user, action, key).Scan(&r.Hash, &answer)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	r.Answer = answer
	return r, err == nil, err
}

func (s Postgres) SaveReplay(ctx context.Context, r ontology.Replay) (ontology.Replay, error) {
	if _, err := s.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.ontology_replays (tenant_id,user_id,action,key,hash,answer) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, s.Schema),
		r.Tenant, r.User, r.Action, r.Key, r.Hash, string(r.Answer)); err != nil {
		return ontology.Replay{}, err
	}
	kept, _, err := s.GetReplay(ctx, r.Tenant, r.User, r.Action, r.Key)
	return kept, err
}
