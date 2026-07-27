package mission

import (
	"context"
	"encoding/json"
	"fmt"
)

// MarkMissionNeedsAttention transitions a running (or verifying) Mission to
// needs_attention, signaling that a human must intervene — e.g. an
// Integration Task hit a merge conflict or a failing joint test suite that
// nothing in this increment can resolve automatically. It is a no-op if the
// Mission is already needs_attention, and an error for any other status.
func (s *Store) MarkMissionNeedsAttention(ctx context.Context, missionID, reason string) (Mission, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mission{}, fmt.Errorf("begin mission attention transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	mission, err := scanMission(tx.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`, missionID))
	if err != nil {
		return Mission{}, wrapMissionNotFound(missionID, err)
	}
	if mission.Status == MissionNeedsAttention {
		return mission, nil
	}
	if !mission.Status.CanTransitionTo(MissionNeedsAttention) {
		return Mission{}, fmt.Errorf(
			"%w: mission %s cannot move from %s to %s",
			ErrInvalidTransition, missionID, mission.Status, MissionNeedsAttention,
		)
	}
	now := s.currentTime()
	if _, err := tx.ExecContext(ctx,
		`UPDATE missions SET status = ?, updated_at = ? WHERE id = ?`,
		MissionNeedsAttention, unixMillis(now), missionID,
	); err != nil {
		return Mission{}, fmt.Errorf("mark mission needs attention: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"reason": reason})
	if err != nil {
		return Mission{}, fmt.Errorf("marshal mission.needs_attention event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID: newID(), MissionID: missionID, Type: "mission.needs_attention",
		Payload: payload, CreatedAt: now,
	}); err != nil {
		return Mission{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mission{}, fmt.Errorf("commit mission attention transition: %w", err)
	}
	mission.Status = MissionNeedsAttention
	mission.UpdatedAt = now
	return mission, nil
}
