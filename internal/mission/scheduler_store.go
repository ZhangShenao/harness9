package mission

import (
	"context"
	"encoding/json"
	"fmt"
)

// ListSchedulableTasks returns queued Tasks whose Mission is eligible to run
// and whose Plan version matches the Mission's current approved Plan version.
// Results are ordered oldest first so Wave-style dispatch processes Tasks FIFO.
func (s *Store) ListSchedulableTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tasks.id, tasks.mission_id, tasks.title, COALESCE(tasks.client_id, ''),
		       tasks.position, tasks.contract, tasks.contract_kind, tasks.status,
		       tasks.created_at, tasks.updated_at
		FROM tasks
		JOIN missions ON missions.id = tasks.mission_id
		WHERE tasks.status = ?
		  AND tasks.plan_version = missions.current_plan_version
		  AND missions.status IN (?, ?)
		ORDER BY tasks.created_at ASC, tasks.id ASC`,
		TaskQueued, MissionReady, MissionRunning)
	if err != nil {
		return nil, fmt.Errorf("list schedulable tasks: %w", err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
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
	return tasks, nil
}

// ActiveTaskCounts returns the number of Tasks with an in-flight lease (leased
// or running) grouped by Mission ID, plus the global total across all Missions.
func (s *Store) ActiveTaskCounts(ctx context.Context) (map[string]int, int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT mission_id, COUNT(*)
		FROM tasks
		WHERE status IN (?, ?)
		GROUP BY mission_id`,
		TaskLeased, TaskRunning)
	if err != nil {
		return nil, 0, fmt.Errorf("count active tasks: %w", err)
	}
	defer rows.Close()
	perMission := make(map[string]int)
	total := 0
	for rows.Next() {
		var missionID string
		var count int
		if err := rows.Scan(&missionID, &count); err != nil {
			return nil, 0, fmt.Errorf("scan active task count: %w", err)
		}
		perMission[missionID] = count
		total += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate active task counts: %w", err)
	}
	return perMission, total, nil
}

// MarkMissionRunning transitions a ready Mission into running the first time
// the Scheduler dispatches into it. Calling it again on an already-running
// Mission is a no-op so the Scheduler can call it unconditionally.
func (s *Store) MarkMissionRunning(ctx context.Context, missionID string) (Mission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mission{}, fmt.Errorf("begin mission running transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	mission, err := scanMission(tx.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`, missionID))
	if err != nil {
		return Mission{}, wrapMissionNotFound(missionID, err)
	}
	if mission.Status == MissionRunning {
		return mission, nil
	}
	if !mission.Status.CanTransitionTo(MissionRunning) {
		return Mission{}, fmt.Errorf(
			"%w: mission %s cannot move from %s to %s",
			ErrInvalidTransition, missionID, mission.Status, MissionRunning,
		)
	}
	now := s.currentTime()
	if _, err := tx.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
		MissionRunning, unixMillis(now), missionID,
	); err != nil {
		return Mission{}, fmt.Errorf("mark mission running: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"from": mission.Status, "to": MissionRunning})
	if err != nil {
		return Mission{}, fmt.Errorf("marshal mission.running event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID: newID(), MissionID: missionID, Type: "mission.running",
		Payload: payload, CreatedAt: now,
	}); err != nil {
		return Mission{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mission{}, fmt.Errorf("commit mission running transition: %w", err)
	}
	mission.Status = MissionRunning
	mission.UpdatedAt = now
	return mission, nil
}
