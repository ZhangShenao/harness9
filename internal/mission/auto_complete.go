package mission

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// GetMission reads a Mission by ID.
func (s *Store) GetMission(ctx context.Context, id string) (Mission, error) {
	var m Mission
	var createdAt, updatedAt int64
	var policyJSON, acceptanceContract, currentPlanVersion sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, goal, status, policy_json, acceptance_contract, current_plan_version, created_at, updated_at
		FROM missions WHERE id = ?`, id).
		Scan(&m.ID, &m.Goal, &m.Status, &policyJSON, &acceptanceContract, &currentPlanVersion, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return Mission{}, ErrNotFound
	}
	if err != nil {
		return Mission{}, fmt.Errorf("get mission: %w", err)
	}
	m.PolicyJSON = policyJSON.String
	m.AcceptanceContract = acceptanceContract.String
	m.CurrentPlanVersion = currentPlanVersion.String
	m.CreatedAt = fromUnixMillis(createdAt)
	m.UpdatedAt = fromUnixMillis(updatedAt)
	return m, nil
}

// TransitionMission validates and applies a Mission lifecycle transition.
func (s *Store) TransitionMission(ctx context.Context, id string, next MissionStatus) (Mission, error) {
	current, err := s.GetMission(ctx, id)
	if err != nil {
		return Mission{}, err
	}
	if !validMissionTransition(current.Status, next) {
		return Mission{}, fmt.Errorf("%w: mission %s cannot move from %s to %s", ErrInvalidTransition, id, current.Status, next)
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`, next, unixMillis(now), id); err != nil {
		return Mission{}, fmt.Errorf("update mission status: %w", err)
	}
	return s.GetMission(ctx, id)
}

// TryCompleteMission checks if all tasks in a mission have succeeded and,
// if so, transitions the mission to verifying then succeeded.
// Returns true if the mission was completed.
func (s *Store) TryCompleteMission(ctx context.Context, missionID string) (bool, error) {
	tasks, err := s.ListTasks(ctx, missionID)
	if err != nil {
		return false, fmt.Errorf("list tasks for completion check: %w", err)
	}
	for _, task := range tasks {
		if task.Status != TaskSucceeded {
			return false, nil
		}
	}
	m, err := s.GetMission(ctx, missionID)
	if err != nil {
		return false, err
	}
	if m.Status != MissionRunning && m.Status != MissionVerifying {
		return false, nil
	}
	if m.Status == MissionRunning {
		if _, err := s.TransitionMission(ctx, missionID, MissionVerifying); err != nil {
			return false, fmt.Errorf("transition to verifying: %w", err)
		}
	}
	if _, err := s.TransitionMission(ctx, missionID, MissionSucceeded); err != nil {
		return false, fmt.Errorf("transition to succeeded: %w", err)
	}
	return true, nil
}
