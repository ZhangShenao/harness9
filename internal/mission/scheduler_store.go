package mission

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListSchedulableTasks returns tasks that are queued, have all dependencies
// satisfied, and belong to a mission with an active (approved) plan version.
func (s *Store) ListSchedulableTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.mission_id, t.title, t.status, t.created_at, t.updated_at, t.contract_kind
		FROM tasks t
		JOIN missions m ON m.id = t.mission_id
		WHERE t.status = ? AND m.current_plan_version IS NOT NULL AND m.current_plan_version != ''
		ORDER BY t.created_at`, TaskQueued)
	if err != nil {
		return nil, fmt.Errorf("list schedulable tasks: %w", err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		var createdAt, updatedAt int64
		var contractKind sql.NullString
		if err := rows.Scan(&task.ID, &task.MissionID, &task.Title, &task.Status, &createdAt, &updatedAt, &contractKind); err != nil {
			return nil, fmt.Errorf("scan schedulable task: %w", err)
		}
		task.CreatedAt = fromUnixMillis(createdAt)
		task.UpdatedAt = fromUnixMillis(updatedAt)
		if contractKind.Valid {
			task.ContractKind = ContractKind(contractKind.String)
		} else {
			task.ContractKind = ContractImplementation
		}
		task.DependsOn, err = s.taskDependencies(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedulable tasks: %w", err)
	}
	var ready []Task
	for _, task := range tasks {
		if s.depsSatisfied(ctx, task.DependsOn) {
			ready = append(ready, task)
		}
	}
	return ready, nil
}

func (s *Store) depsSatisfied(ctx context.Context, depIDs []string) bool {
	for _, depID := range depIDs {
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, depID).Scan(&status)
		if err != nil || status != string(TaskSucceeded) {
			return false
		}
	}
	return true
}

// ActiveTaskCounts returns per-mission and global in-flight task counts.
// The global count is under key "__global__".
func (s *Store) ActiveTaskCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.mission_id, COUNT(*)
		FROM tasks t
		WHERE t.status IN ('leased', 'running')
		GROUP BY t.mission_id`)
	if err != nil {
		return nil, fmt.Errorf("active task counts: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	var global int
	for rows.Next() {
		var missionID string
		var count int
		if err := rows.Scan(&missionID, &count); err != nil {
			return nil, fmt.Errorf("scan task count: %w", err)
		}
		counts[missionID] = count
		global += count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task counts: %w", err)
	}
	counts["__global__"] = global
	return counts, nil
}

// MarkMissionRunning idempotently transitions a ready mission to running.
func (s *Store) MarkMissionRunning(ctx context.Context, missionID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ? AND status IN (?, ?)`,
		MissionRunning, unixMillis(now), missionID, MissionReady, MissionRunning)
	if err != nil {
		return fmt.Errorf("mark mission running: %w", err)
	}
	return nil
}

// MarkAttemptFinished records the final status and timing of an attempt.
func (s *Store) MarkAttemptFinished(ctx context.Context, attemptID, status, exitReason string) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_attempts SET status = ?, exit_reason = ?, finished_at = ?, updated_at = ? WHERE id = ?`,
		status, exitReason, unixMillis(now), unixMillis(now), attemptID)
	if err != nil {
		return fmt.Errorf("mark attempt finished: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetLatestAttempt returns the most recent attempt for a task.
func (s *Store) GetLatestAttempt(ctx context.Context, taskID string) (TaskAttempt, error) {
	var a TaskAttempt
	var createdAt, updatedAt int64
	var startedAt, finishedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, worker, status, created_at, updated_at
		FROM task_attempts WHERE task_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, taskID).
		Scan(&a.ID, &a.TaskID, &a.Worker, &a.Status, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return TaskAttempt{}, ErrNotFound
	}
	if err != nil {
		return TaskAttempt{}, fmt.Errorf("get latest attempt: %w", err)
	}
	a.CreatedAt = fromUnixMillis(createdAt)
	a.UpdatedAt = fromUnixMillis(updatedAt)
	if startedAt.Valid {
		t := fromUnixMillis(startedAt.Int64)
		a.StartedAt = &t
	}
	if finishedAt.Valid {
		t := fromUnixMillis(finishedAt.Int64)
		a.FinishedAt = &t
	}
	return a, nil
}
