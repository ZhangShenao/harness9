package mission

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AcquireLease creates an exclusive active lease for a Task.
// The unique partial index idx_workspace_leases_active_task enforces that only
// one active lease may exist per task at a time; a duplicate acquire fails.
func (s *Store) AcquireLease(ctx context.Context, taskID, path, branch, sandboxID string, ttl time.Duration) (WorkspaceLease, error) {
	now := time.Now().UTC()
	lease := WorkspaceLease{
		ID:        newID(),
		TaskID:    taskID,
		Path:      path,
		Branch:    branch,
		SandboxID: sandboxID,
		Status:    LeaseActive,
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_leases (id, task_id, path, branch, sandbox_id, status, expires_at, created_at, released_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		lease.ID, lease.TaskID, lease.Path, lease.Branch, lease.SandboxID,
		lease.Status, unixMillis(lease.ExpiresAt), unixMillis(now))
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("acquire lease (task may already have one): %w", err)
	}
	return lease, nil
}

// ReleaseLease marks an active lease as released. Returns ErrNotFound if the
// lease does not exist or is no longer active.
func (s *Store) ReleaseLease(ctx context.Context, leaseID string) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspace_leases SET status = ?, released_at = ? WHERE id = ? AND status = ?`,
		LeaseReleased, unixMillis(now), leaseID, LeaseActive)
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetActiveLease returns the active lease for a Task, or ErrNotFound if none exists.
func (s *Store) GetActiveLease(ctx context.Context, taskID string) (WorkspaceLease, error) {
	var lease WorkspaceLease
	var expiresAt, createdAt int64
	var releasedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, path, branch, sandbox_id, status, expires_at, created_at, released_at
		FROM workspace_leases WHERE task_id = ? AND status = ?`, taskID, LeaseActive).
		Scan(&lease.ID, &lease.TaskID, &lease.Path, &lease.Branch, &lease.SandboxID,
			&lease.Status, &expiresAt, &createdAt, &releasedAt)
	if err == sql.ErrNoRows {
		return WorkspaceLease{}, ErrNotFound
	}
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("get active lease: %w", err)
	}
	lease.ExpiresAt = fromUnixMillis(expiresAt)
	lease.CreatedAt = fromUnixMillis(createdAt)
	if releasedAt.Valid {
		t := fromUnixMillis(releasedAt.Int64)
		lease.ReleasedAt = &t
	}
	return lease, nil
}

// ExpireLeases marks active leases past their expiry as expired.
// Returns the number of leases transitioned to expired.
func (s *Store) ExpireLeases(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspace_leases SET status = ? WHERE status = ? AND expires_at < ?`,
		LeaseExpired, LeaseActive, unixMillis(now))
	if err != nil {
		return 0, fmt.Errorf("expire leases: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
