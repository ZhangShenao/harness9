package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetAttempt reads one durable Task execution attempt.
func (s *Store) GetAttempt(ctx context.Context, id string) (*TaskAttempt, error) {
	attempt, err := scanAttempt(s.db.QueryRowContext(ctx, `
		SELECT id, task_id, COALESCE(lease_id, ''), worker, status, created_at, updated_at
		FROM task_attempts
		WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// TransitionAttempt validates and persists one Attempt lifecycle transition.
func (s *Store) TransitionAttempt(
	ctx context.Context,
	id string,
	next AttemptStatus,
) (*TaskAttempt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin attempt transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	attempt, err := scanAttempt(tx.QueryRowContext(ctx, `
		SELECT id, task_id, COALESCE(lease_id, ''), worker, status, created_at, updated_at
		FROM task_attempts
		WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if !attempt.Status.CanTransitionTo(next) {
		return nil, fmt.Errorf(
			"%w: attempt %s cannot move from %s to %s",
			ErrInvalidTransition,
			id,
			attempt.Status,
			next,
		)
	}
	var missionID string
	if err := tx.QueryRowContext(ctx,
		`SELECT mission_id FROM tasks WHERE id = ?`, attempt.TaskID,
	).Scan(&missionID); err != nil {
		return nil, fmt.Errorf("read attempt mission: %w", err)
	}
	now := s.currentTime()
	if _, err := tx.ExecContext(ctx, `
		UPDATE task_attempts
		SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		next,
		unixMillis(now),
		attempt.ID,
		attempt.Status,
	); err != nil {
		return nil, fmt.Errorf("update attempt status: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"from": attempt.Status,
		"to":   next,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal attempt.transitioned event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: missionID,
		TaskID:    attempt.TaskID,
		AttemptID: attempt.ID,
		Type:      "attempt.transitioned",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return nil, err
	}
	attempt.Status = next
	attempt.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit attempt transition: %w", err)
	}
	return &attempt, nil
}

// AcquireLease grants one Task exclusive use of a persisted workspace.
func (s *Store) AcquireLease(
	ctx context.Context,
	taskID string,
	path string,
	branch string,
	sandboxID string,
	ttl time.Duration,
) (WorkspaceLease, error) {
	taskID = strings.TrimSpace(taskID)
	path = strings.TrimSpace(path)
	branch = strings.TrimSpace(branch)
	sandboxID = strings.TrimSpace(sandboxID)
	if taskID == "" {
		return WorkspaceLease{}, fmt.Errorf("lease task ID is required")
	}
	if path == "" {
		return WorkspaceLease{}, fmt.Errorf("lease path is required")
	}
	if ttl <= 0 {
		return WorkspaceLease{}, fmt.Errorf("lease TTL must be positive")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("begin lease transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var missionID string
	var status TaskStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT mission_id, status FROM tasks WHERE id = ?`, taskID,
	).Scan(&missionID, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkspaceLease{}, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
		}
		return WorkspaceLease{}, fmt.Errorf("read lease task: %w", err)
	}
	if status != TaskQueued && status != TaskLeased {
		return WorkspaceLease{}, fmt.Errorf(
			"%w: task %s cannot acquire a lease from %s",
			ErrInvalidTransition,
			taskID,
			status,
		)
	}

	now := s.currentTime()
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_leases
		SET status = 'expired', released_at = ?
		WHERE task_id = ?
		  AND status IN ('active', 'releasing')
		  AND expires_at <= ?`,
		unixMillis(now),
		taskID,
		unixMillis(now),
	); err != nil {
		return WorkspaceLease{}, fmt.Errorf("expire stale task leases: %w", err)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM workspace_leases
		WHERE task_id = ? AND status IN ('active', 'releasing')`,
		taskID,
	).Scan(&active); err != nil {
		return WorkspaceLease{}, fmt.Errorf("check active task lease: %w", err)
	}
	if active != 0 {
		return WorkspaceLease{}, fmt.Errorf("%w: task %s already has an active lease", ErrConflict, taskID)
	}
	if status == TaskQueued {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			TaskLeased,
			unixMillis(now),
			taskID,
			TaskQueued,
		); err != nil {
			return WorkspaceLease{}, fmt.Errorf("mark task leased: %w", err)
		}
	}

	lease := WorkspaceLease{
		ID:        newID(),
		TaskID:    taskID,
		Path:      path,
		Branch:    branch,
		SandboxID: sandboxID,
		Status:    "active",
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_leases (
			id, task_id, path, branch, sandbox_id, status,
			expires_at, created_at, released_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		lease.ID,
		lease.TaskID,
		lease.Path,
		lease.Branch,
		lease.SandboxID,
		lease.Status,
		unixMillis(lease.ExpiresAt),
		unixMillis(lease.CreatedAt),
	); err != nil {
		return WorkspaceLease{}, fmt.Errorf("insert workspace lease: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"lease_id":   lease.ID,
		"path":       lease.Path,
		"branch":     lease.Branch,
		"sandbox_id": lease.SandboxID,
		"expires_at": unixMillis(lease.ExpiresAt),
	})
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("marshal lease.acquired event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: missionID,
		TaskID:    taskID,
		Type:      "lease.acquired",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return WorkspaceLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceLease{}, fmt.Errorf("commit workspace lease: %w", err)
	}
	return lease, nil
}

// ReleaseLease marks an active workspace Lease terminal and appends an audit event.
func (s *Store) ReleaseLease(ctx context.Context, id string) (WorkspaceLease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("begin lease release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lease, missionID, err := getLeaseWithMission(ctx, tx, id)
	if err != nil {
		return WorkspaceLease{}, err
	}
	if lease.Status != "active" && lease.Status != "releasing" {
		return WorkspaceLease{}, fmt.Errorf(
			"%w: lease %s is already %s",
			ErrConflict,
			id,
			lease.Status,
		)
	}
	now := s.currentTime()
	terminalStatus := "released"
	if !lease.ExpiresAt.After(now) {
		terminalStatus = "expired"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspace_leases
		SET status = ?, released_at = ?
		WHERE id = ? AND status IN ('active', 'releasing')`,
		terminalStatus,
		unixMillis(now),
		lease.ID,
	); err != nil {
		return WorkspaceLease{}, fmt.Errorf("release workspace lease: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"lease_id": lease.ID,
		"status":   terminalStatus,
	})
	if err != nil {
		return WorkspaceLease{}, fmt.Errorf("marshal lease.released event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: missionID,
		TaskID:    lease.TaskID,
		Type:      "lease.released",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return WorkspaceLease{}, err
	}
	lease.Status = terminalStatus
	lease.ReleasedAt = &now
	if err := tx.Commit(); err != nil {
		return WorkspaceLease{}, fmt.Errorf("commit lease release: %w", err)
	}
	return lease, nil
}

// ListRecoverableAttempts returns Attempts awaiting post-restart reconciliation.
func (s *Store) ListRecoverableAttempts(ctx context.Context) ([]TaskAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, COALESCE(lease_id, ''), worker, status, created_at, updated_at
		FROM task_attempts
		WHERE status = ?
		ORDER BY created_at, id`,
		AttemptIndeterminate,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable attempts: %w", err)
	}
	defer rows.Close()
	var attempts []TaskAttempt
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable attempts: %w", err)
	}
	return attempts, nil
}

// ListEvents returns one Mission's append-only events in stable cursor order.
func (s *Store) ListEvents(
	ctx context.Context,
	missionID string,
	after any,
	limit int,
) ([]Event, error) {
	missionID = strings.TrimSpace(missionID)
	if missionID == "" {
		return nil, fmt.Errorf("event mission ID is required")
	}
	var afterID string
	var afterCreatedAt int64
	switch value := after.(type) {
	case nil:
	case string:
		afterID = strings.TrimSpace(value)
	case int:
		afterCreatedAt = int64(value)
	case int64:
		afterCreatedAt = value
	default:
		return nil, fmt.Errorf("event cursor must be an event ID or millisecond timestamp")
	}
	if afterCreatedAt < 0 {
		return nil, fmt.Errorf("event cursor cannot be negative")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	var rows *sql.Rows
	var err error
	switch {
	case afterID != "":
		var cursorCreatedAt int64
		if err := s.db.QueryRowContext(ctx, `
			SELECT created_at
			FROM mission_events
			WHERE mission_id = ? AND id = ?`,
			missionID,
			afterID,
		).Scan(&cursorCreatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("event %s: %w", afterID, ErrNotFound)
			}
			return nil, fmt.Errorf("read event cursor: %w", err)
		}
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, mission_id, task_id, attempt_id, type, payload, created_at
			FROM mission_events
			WHERE mission_id = ?
			  AND (created_at > ? OR (created_at = ? AND id > ?))
			ORDER BY created_at, id
			LIMIT ?`,
			missionID,
			cursorCreatedAt,
			cursorCreatedAt,
			afterID,
			limit,
		)
	case afterCreatedAt > 0:
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, mission_id, task_id, attempt_id, type, payload, created_at
			FROM mission_events
			WHERE mission_id = ? AND created_at > ?
			ORDER BY created_at, id
			LIMIT ?`,
			missionID,
			afterCreatedAt,
			limit,
		)
	default:
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, mission_id, task_id, attempt_id, type, payload, created_at
			FROM mission_events
			WHERE mission_id = ?
			ORDER BY created_at, id
			LIMIT ?`,
			missionID,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list mission events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var createdAt int64
		if err := rows.Scan(
			&event.ID,
			&event.MissionID,
			&event.TaskID,
			&event.AttemptID,
			&event.Type,
			&event.Payload,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan mission event: %w", err)
		}
		event.CreatedAt = fromUnixMillis(createdAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mission events: %w", err)
	}
	return events, nil
}

// MarkInterruptedAttemptsIndeterminate marks active execution as requiring reconciliation.
func (s *Store) MarkInterruptedAttemptsIndeterminate(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin interrupted attempt recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT attempt.id, attempt.task_id, task.mission_id
		FROM task_attempts attempt
		JOIN tasks task ON task.id = attempt.task_id
		WHERE attempt.status = ?
		  AND task.status IN (?, ?)
		ORDER BY attempt.created_at, attempt.id`,
		AttemptRunning,
		TaskLeased,
		TaskRunning,
	)
	if err != nil {
		return 0, fmt.Errorf("list interrupted attempts: %w", err)
	}
	type interruptedAttempt struct {
		id        string
		taskID    string
		missionID string
	}
	var attempts []interruptedAttempt
	for rows.Next() {
		var attempt interruptedAttempt
		if err := rows.Scan(&attempt.id, &attempt.taskID, &attempt.missionID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan interrupted attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate interrupted attempts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close interrupted attempts: %w", err)
	}

	now := s.currentTime()
	for _, attempt := range attempts {
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_attempts
			SET status = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			AttemptIndeterminate,
			unixMillis(now),
			attempt.id,
			AttemptRunning,
		); err != nil {
			return 0, fmt.Errorf("mark attempt indeterminate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = ?, updated_at = ?
			WHERE id = ? AND status IN (?, ?)`,
			TaskIndeterminate,
			unixMillis(now),
			attempt.taskID,
			TaskLeased,
			TaskRunning,
		); err != nil {
			return 0, fmt.Errorf("mark interrupted task indeterminate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE workspace_leases
			SET status = 'expired', released_at = ?
			WHERE task_id = ? AND status IN ('active', 'releasing')`,
			unixMillis(now),
			attempt.taskID,
		); err != nil {
			return 0, fmt.Errorf("expire interrupted task leases: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"reason": "runtime_restart",
			"status": AttemptIndeterminate,
		})
		if err != nil {
			return 0, fmt.Errorf("marshal attempt.indeterminate event: %w", err)
		}
		if err := insertEvent(ctx, tx, Event{
			ID:        newID(),
			MissionID: attempt.missionID,
			TaskID:    attempt.taskID,
			AttemptID: attempt.id,
			Type:      "attempt.indeterminate",
			Payload:   payload,
			CreatedAt: now,
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit interrupted attempt recovery: %w", err)
	}
	return len(attempts), nil
}

func scanAttempt(row rowScanner) (TaskAttempt, error) {
	var attempt TaskAttempt
	var createdAt, updatedAt int64
	if err := row.Scan(
		&attempt.ID,
		&attempt.TaskID,
		&attempt.LeaseID,
		&attempt.Worker,
		&attempt.Status,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TaskAttempt{}, ErrNotFound
		}
		return TaskAttempt{}, fmt.Errorf("scan task attempt: %w", err)
	}
	attempt.CreatedAt = fromUnixMillis(createdAt)
	attempt.UpdatedAt = fromUnixMillis(updatedAt)
	return attempt, nil
}

func getLeaseWithMission(
	ctx context.Context,
	q queryRower,
	id string,
) (WorkspaceLease, string, error) {
	var lease WorkspaceLease
	var missionID string
	var expiresAt, createdAt int64
	var releasedAt sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT lease.id, lease.task_id, lease.path, lease.branch, lease.sandbox_id,
		       lease.status, lease.expires_at, lease.created_at, lease.released_at,
		       task.mission_id
		FROM workspace_leases lease
		JOIN tasks task ON task.id = lease.task_id
		WHERE lease.id = ?`,
		id,
	).Scan(
		&lease.ID,
		&lease.TaskID,
		&lease.Path,
		&lease.Branch,
		&lease.SandboxID,
		&lease.Status,
		&expiresAt,
		&createdAt,
		&releasedAt,
		&missionID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceLease{}, "", ErrNotFound
	}
	if err != nil {
		return WorkspaceLease{}, "", fmt.Errorf("read workspace lease: %w", err)
	}
	lease.ExpiresAt = fromUnixMillis(expiresAt)
	lease.CreatedAt = fromUnixMillis(createdAt)
	if releasedAt.Valid {
		value := fromUnixMillis(releasedAt.Int64)
		lease.ReleasedAt = &value
	}
	return lease, missionID, nil
}
