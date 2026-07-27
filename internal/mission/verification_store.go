package mission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetLatestLease returns the most recently created WorkspaceLease for a Task,
// regardless of its current status (active, released, or expired). Verifier
// Tasks use this to find the worktree/branch an implementation Task's Worker
// used, so they can check out the same commit independently.
func (s *Store) GetLatestLease(ctx context.Context, taskID string) (WorkspaceLease, error) {
	var lease WorkspaceLease
	var expiresAt, createdAt int64
	var releasedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, path, branch, sandbox_id, status, expires_at, created_at, released_at
		FROM workspace_leases
		WHERE task_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, taskID,
	).Scan(
		&lease.ID, &lease.TaskID, &lease.Path, &lease.Branch, &lease.SandboxID,
		&lease.Status, &expiresAt, &createdAt, &releasedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceLease{}, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("get latest lease: %w", err)
	}
	lease.ExpiresAt = fromUnixMillis(expiresAt)
	lease.CreatedAt = fromUnixMillis(createdAt)
	if releasedAt.Valid {
		value := fromUnixMillis(releasedAt.Int64)
		lease.ReleasedAt = &value
	}
	return lease, nil
}

// GetLatestAttempt returns the most recently created TaskAttempt for a Task.
// Verifier Tasks use this to find the AttemptID a passing/failing Evidence
// record should reference as the artifact producer (Evidence.AttemptID),
// distinct from the verifying Attempt itself (Evidence.VerifierAttemptID).
func (s *Store) GetLatestAttempt(ctx context.Context, taskID string) (TaskAttempt, error) {
	attempt, err := scanAttempt(s.db.QueryRowContext(ctx, `
		SELECT id, task_id, COALESCE(lease_id, ''), worker, status, created_at, updated_at
		FROM task_attempts
		WHERE task_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, taskID))
	if err != nil {
		return TaskAttempt{}, fmt.Errorf("get latest attempt for task %s: %w", taskID, err)
	}
	return attempt, nil
}
