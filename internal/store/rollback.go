package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------- stack rollback points ----------------

// RollbackKeep is how many rollback points a stack keeps.
const RollbackKeep = 5

// RollbackPoint is what a stack's services ran right before a deploy,
// Pull & redeploy, automatic update or rollback.
type RollbackPoint struct {
	ID          string                     `json:"id"`
	StackID     string                     `json:"stackId"`
	CreatedAt   time.Time                  `json:"createdAt"`
	Trigger     string                     `json:"trigger"` // deploy, pull, auto, rollback
	CreatedBy   string                     `json:"createdBy"`
	ComposeHash string                     `json:"composeHash"`
	Services    map[string]RollbackService `json:"services"`
}

// RollbackService is the image one service ran.
type RollbackService struct {
	Ref     string `json:"ref"`
	ImageID string `json:"imageId"`
	Digest  string `json:"digest,omitempty"`
	Created string `json:"created,omitempty"`
}

const rollbackCols = `id::text, stack_id::text, created_at, trigger, created_by, compose_hash, services`

func scanRollback(row pgx.Row) (RollbackPoint, error) {
	var p RollbackPoint
	var svcs []byte
	err := row.Scan(&p.ID, &p.StackID, &p.CreatedAt, &p.Trigger, &p.CreatedBy, &p.ComposeHash, &svcs)
	if err == nil {
		err = json.Unmarshal(svcs, &p.Services)
	}
	if p.Services == nil {
		p.Services = map[string]RollbackService{}
	}
	return p, notFound(err)
}

// AddRollbackPoint saves a point and drops the stack's points beyond the
// newest RollbackKeep.
func (s *Store) AddRollbackPoint(ctx context.Context, p RollbackPoint) (RollbackPoint, error) {
	svcs, _ := json.Marshal(p.Services)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RollbackPoint{}, err
	}
	defer tx.Rollback(ctx)
	out, err := scanRollback(tx.QueryRow(ctx,
		`INSERT INTO stack_rollback_points (stack_id, trigger, created_by, compose_hash, services) VALUES ($1::uuid, $2, $3, $4, $5::jsonb) RETURNING `+rollbackCols,
		p.StackID, p.Trigger, p.CreatedBy, p.ComposeHash, svcs))
	if err != nil {
		return RollbackPoint{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM stack_rollback_points WHERE stack_id = $1::uuid AND id NOT IN (
		SELECT id FROM stack_rollback_points WHERE stack_id = $1::uuid ORDER BY created_at DESC, id DESC LIMIT $2)`, p.StackID, RollbackKeep); err != nil {
		return RollbackPoint{}, err
	}
	return out, tx.Commit(ctx)
}

// LatestRollbackPoint is a stack's newest point (ErrNotFound if none).
func (s *Store) LatestRollbackPoint(ctx context.Context, stackID string) (RollbackPoint, error) {
	return scanRollback(s.db.QueryRow(ctx, `SELECT `+rollbackCols+` FROM stack_rollback_points WHERE stack_id = $1::uuid ORDER BY created_at DESC, id DESC LIMIT 1`, stackID))
}

// SetStackRolledBack records that a stack runs the images of the point
// recorded at "to" (nil: it runs its newest images again).
func (s *Store) SetStackRolledBack(ctx context.Context, id string, to *time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE stacks SET rolled_back_to = $2 WHERE id = $1::uuid`, id, to)
	return err
}
