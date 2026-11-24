package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------- automatic updates ----------------

// AutoUpdateTarget is a stack or standalone container opted in to
// automatic updates, with the outcome of its last attempt.
type AutoUpdateTarget struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // "stack" or "container"
	// StackID and StackName are set for stacks; ContainerName for containers.
	StackID       string `json:"stackId,omitempty"`
	StackName     string `json:"stackName,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	// HostID is the container's host, or the stack's current host.
	HostID     string     `json:"hostId"`
	HostName   string     `json:"hostName"`
	EnabledBy  string     `json:"enabledBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastRunAt  *time.Time `json:"lastRunAt"`
	LastResult string     `json:"lastResult"` // ok, failed, skipped; "" before the first run
	LastError  string     `json:"lastError"`  // the error, or why it was skipped
	// AutoRollback: roll a stack back right away if its update fails.
	AutoRollback bool `json:"autoRollback"`
}

// Name is what the target is called in the UI and the audit log.
func (t AutoUpdateTarget) Name() string {
	if t.Kind == "stack" {
		return t.StackName
	}
	return t.ContainerName
}

const autoUpdateCols = `t.id::text, t.kind, COALESCE(t.stack_id::text, ''), COALESCE(s.name, ''), COALESCE(t.container_name, ''),
	COALESCE(t.host_id, s.host_id)::text, COALESCE(h.name, ''), t.enabled_by, t.created_at, t.last_run_at, t.last_result, t.last_error, t.auto_rollback`

// A stack's host is wherever the stack is now (it may have been moved).
const autoUpdateFrom = ` FROM auto_update_targets t
	LEFT JOIN stacks s ON s.id = t.stack_id
	LEFT JOIN hosts h ON h.id = COALESCE(t.host_id, s.host_id)`

func scanAutoUpdate(row pgx.Row) (AutoUpdateTarget, error) {
	var t AutoUpdateTarget
	err := row.Scan(&t.ID, &t.Kind, &t.StackID, &t.StackName, &t.ContainerName, &t.HostID, &t.HostName, &t.EnabledBy, &t.CreatedAt, &t.LastRunAt, &t.LastResult, &t.LastError, &t.AutoRollback)
	return t, notFound(err)
}

// ListAutoUpdateTargets lists every opted-in target, by host then name.
func (s *Store) ListAutoUpdateTargets(ctx context.Context) ([]AutoUpdateTarget, error) {
	rows, err := s.db.Query(ctx, `SELECT `+autoUpdateCols+autoUpdateFrom+` ORDER BY h.name, COALESCE(s.name, t.container_name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutoUpdateTarget
	for rows.Next() {
		t, err := scanAutoUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EnableStackAutoUpdate opts a stack in. Doing it again changes nothing.
func (s *Store) EnableStackAutoUpdate(ctx context.Context, stackID, by string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO auto_update_targets (kind, stack_id, enabled_by) VALUES ('stack', $1::uuid, $2)
		ON CONFLICT (stack_id) WHERE kind = 'stack' DO NOTHING`, stackID, by)
	return err
}

// EnableContainerAutoUpdate opts a standalone container in. Doing it again
// changes nothing.
func (s *Store) EnableContainerAutoUpdate(ctx context.Context, hostID, name, by string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO auto_update_targets (kind, host_id, container_name, enabled_by) VALUES ('container', $1::uuid, $2, $3)
		ON CONFLICT (host_id, container_name) WHERE kind = 'container' DO NOTHING`, hostID, name, by)
	return err
}

// SetStackAutoRollback turns "roll back automatically if the update
// fails" on or off for an opted-in stack; false if it isn't opted in.
func (s *Store) SetStackAutoRollback(ctx context.Context, stackID string, on bool) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE auto_update_targets SET auto_rollback = $2 WHERE kind = 'stack' AND stack_id = $1::uuid`, stackID, on)
	return tag.RowsAffected() > 0, err
}

// DisableStackAutoUpdate opts a stack out; false if it wasn't opted in.
func (s *Store) DisableStackAutoUpdate(ctx context.Context, stackID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM auto_update_targets WHERE kind = 'stack' AND stack_id = $1::uuid`, stackID)
	return tag.RowsAffected() > 0, err
}

// DisableContainerAutoUpdate opts a container out; false if it wasn't opted in.
func (s *Store) DisableContainerAutoUpdate(ctx context.Context, hostID, name string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM auto_update_targets WHERE kind = 'container' AND host_id = $1::uuid AND container_name = $2`, hostID, name)
	return tag.RowsAffected() > 0, err
}

// RecordAutoUpdate saves the outcome of a target's latest attempt.
func (s *Store) RecordAutoUpdate(ctx context.Context, id string, at time.Time, result, errText string) error {
	_, err := s.db.Exec(ctx, `UPDATE auto_update_targets SET last_run_at = $2, last_result = $3, last_error = $4 WHERE id = $1::uuid`, id, at, result, errText)
	return err
}
