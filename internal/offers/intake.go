package offers

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/xiant10/t10-modkit/people"

	"t10-collab/internal/activity"
	"t10-collab/internal/policy"
)

// maxIntakeRefs bounds an intake's units and lists.
const maxIntakeRefs = 50

// Intake is who may offer work to an agent account. Its reporting line can
// always assign it work. With nothing set, nobody else can offer it any.
type Intake struct {
	UserID   string   `json:"user_id"`
	Everyone bool     `json:"everyone"`
	Units    []string `json:"units"`
	Lists    []string `json:"lists"`
}

// GetIntake reads an agent account's intake. One never set is empty.
func (s *Service) GetIntake(ctx context.Context, c policy.Caller, userID string) (Intake, error) {
	in := Intake{UserID: userID, Units: []string{}, Lists: []string{}}
	if !people.IsAgentID(userID) {
		return Intake{}, policy.ErrNotFound
	}
	err := s.pool.QueryRow(ctx, "SELECT everyone, units, lists FROM collab.agent_intake WHERE tenant_id = $1 AND user_id = $2",
		c.TenantID, userID).Scan(&in.Everyone, &in.Units, &in.Lists)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Intake{}, fmt.Errorf("read intake: %w", err)
	}
	return in, nil
}

// SetIntake replaces an agent account's intake, for a module admin. The account
// must be an active agent member, and each list must exist in the tenant. Units
// are the auth service's, and one deleted later simply matches nobody.
func (s *Service) SetIntake(ctx context.Context, c policy.Caller, in Intake) (Intake, error) {
	if len(in.Units) > maxIntakeRefs || len(in.Lists) > maxIntakeRefs {
		return Intake{}, ErrInvalid
	}
	in.Units, in.Lists = compact(in.Units), compact(in.Lists)
	member, err := s.org.Member(ctx, c.TenantID, in.UserID)
	if errors.Is(err, people.ErrNotFound) || (err == nil && (!member.Agent() || !member.Active())) {
		return Intake{}, policy.ErrNotFound
	}
	if err != nil {
		return Intake{}, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var found int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM collab.lists WHERE tenant_id = $1 AND id = ANY($2)",
			c.TenantID, in.Lists).Scan(&found); err != nil {
			return fmt.Errorf("read lists: %w", err)
		}
		if found != len(in.Lists) {
			return ErrInvalid
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO collab.agent_intake (tenant_id, user_id, everyone, units, lists, updated_by) VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, user_id) DO UPDATE SET everyone = $3, units = $4, lists = $5, updated_by = $6, updated_at = now()`,
			c.TenantID, in.UserID, in.Everyone, in.Units, in.Lists, c.UserID); err != nil {
			return fmt.Errorf("write intake: %w", err)
		}
		return activity.Record(ctx, tx, c, activity.Change{
			Kind: "agent.intake_set", Detail: map[string]any{"user_id": in.UserID, "everyone": in.Everyone, "units": in.Units, "lists": in.Lists},
		})
	})
	if err != nil {
		return Intake{}, err
	}
	return in, nil
}

func compact(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		return []string{}
	}
	return out
}
