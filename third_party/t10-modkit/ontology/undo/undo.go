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
	"sort"
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

func (m *Memory) OpenUndos(_ context.Context, tenant, user string, now time.Time) ([]ontology.UndoEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	out := []ontology.UndoEntry{}
	for _, u := range m.entries {
		if u.Tenant == tenant && u.User == user && !u.Undone && now.Before(u.ExpiresAt) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (m *Memory) SetUndone(_ context.Context, tenant, id string, undone bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	u, ok := m.entries[tenant+"|"+id]
	if !ok {
		return ontology.ErrNotFound
	}
	if (undone && u.Undone) || u.Reverted {
		return ontology.ErrUndone
	}
	u.Undone = undone
	m.entries[tenant+"|"+id] = u
	return nil
}

func (m *Memory) FinishUndo(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	u, ok := m.entries[tenant+"|"+id]
	if !ok {
		return ontology.ErrNotFound
	}
	u.Undone, u.Reverted = true, true
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

func (m *Memory) ReserveReplay(_ context.Context, r ontology.Replay) (ontology.Replay, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	k := replayKey(r.Tenant, r.User, r.Action, r.Key)
	if kept, ok := m.replays[k]; ok {
		return kept, false, nil
	}
	r.Answer = nil
	m.replays[k] = r
	return r, true, nil
}

func (m *Memory) FinishReplay(_ context.Context, r ontology.Replay) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.replays[replayKey(r.Tenant, r.User, r.Action, r.Key)] = r
	return nil
}

func (m *Memory) ReleaseReplay(_ context.Context, tenant, user, action, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	k := replayKey(tenant, user, action, key)
	if kept, ok := m.replays[k]; ok && kept.Answer == nil {
		delete(m.replays, k)
	}
	return nil
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
	_, err := s.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.ontology_undo (tenant_id,id,user_id,action,agent,object,payload,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, s.Schema),
		u.Tenant, u.ID, u.User, u.Action, u.Agent, u.Object, payload, u.CreatedAt, u.ExpiresAt)
	return err
}

func (s Postgres) GetUndo(ctx context.Context, tenant, id string) (ontology.UndoEntry, error) {
	var u ontology.UndoEntry
	var payload []byte
	var undone *time.Time
	var reverted *time.Time
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT id,tenant_id,user_id,action,agent,object,payload,created_at,expires_at,undone_at,reverted_at FROM %s.ontology_undo WHERE tenant_id=$1 AND id=$2`, s.Schema), tenant, id).
		Scan(&u.ID, &u.Tenant, &u.User, &u.Action, &u.Agent, &u.Object, &payload, &u.CreatedAt, &u.ExpiresAt, &undone, &reverted)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ontology.ErrNotFound
	}
	u.Payload, u.Undone, u.Reverted = payload, undone != nil, reverted != nil
	return u, err
}

func (s Postgres) OpenUndos(ctx context.Context, tenant, user string, now time.Time) ([]ontology.UndoEntry, error) {
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT id,tenant_id,user_id,action,agent,object,payload,created_at,expires_at FROM %s.ontology_undo
		WHERE tenant_id=$1 AND user_id=$2 AND undone_at IS NULL AND expires_at > $3 ORDER BY created_at DESC, id DESC LIMIT 200`, s.Schema), tenant, user, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ontology.UndoEntry{}
	for rows.Next() {
		var u ontology.UndoEntry
		if err := rows.Scan(&u.ID, &u.Tenant, &u.User, &u.Action, &u.Agent, &u.Object, &u.Payload, &u.CreatedAt, &u.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s Postgres) FinishUndo(ctx context.Context, tenant, id string) error {
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.ontology_undo SET undone_at=coalesce(undone_at, clock_timestamp()), reverted_at=coalesce(reverted_at, clock_timestamp()) WHERE tenant_id=$1 AND id=$2`, s.Schema), tenant, id)
	if err == nil && tag.RowsAffected() == 0 {
		return ontology.ErrNotFound
	}
	return err
}

func (s Postgres) SetUndone(ctx context.Context, tenant, id string, undone bool) error {
	query := `UPDATE %s.ontology_undo SET undone_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND undone_at IS NULL`
	if !undone {
		query = `UPDATE %s.ontology_undo SET undone_at=NULL WHERE tenant_id=$1 AND id=$2 AND reverted_at IS NULL`
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

func (s Postgres) ReserveReplay(ctx context.Context, r ontology.Replay) (ontology.Replay, bool, error) {
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.ontology_replays (tenant_id,user_id,action,key,hash) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, s.Schema),
		r.Tenant, r.User, r.Action, r.Key, r.Hash)
	if err != nil {
		return ontology.Replay{}, false, err
	}
	if tag.RowsAffected() == 1 {
		r.Answer = nil
		return r, true, nil
	}
	kept, found, err := s.GetReplay(ctx, r.Tenant, r.User, r.Action, r.Key)
	if err == nil && !found {
		// Released between the insert and the read: claim it again.
		return s.ReserveReplay(ctx, r)
	}
	return kept, false, err
}

func (s Postgres) FinishReplay(ctx context.Context, r ontology.Replay) error {
	_, err := s.Pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.ontology_replays SET answer=$5 WHERE tenant_id=$1 AND user_id=$2 AND action=$3 AND key=$4`, s.Schema),
		r.Tenant, r.User, r.Action, r.Key, string(r.Answer))
	return err
}

func (s Postgres) ReleaseReplay(ctx context.Context, tenant, user, action, key string) error {
	_, err := s.Pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.ontology_replays WHERE tenant_id=$1 AND user_id=$2 AND action=$3 AND key=$4 AND answer IS NULL`, s.Schema),
		tenant, user, action, key)
	return err
}
