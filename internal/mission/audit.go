package mission

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AddAuditEvent records an immutable audit event for one Command execution.
// MissionID, CommandKind and Actor are required; the event is append-only.
func (s *Store) AddAuditEvent(ctx context.Context, event AuditEvent) (AuditEvent, error) {
	if event.MissionID == "" || event.CommandKind == "" || event.Actor == "" {
		return AuditEvent{}, fmt.Errorf("mission ID, command kind and actor are required")
	}
	event.ID = newID()
	event.CreatedAt = time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, mission_id, command_kind, actor, target, reason, idempotency_key, result, before_state, after_state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.MissionID, event.CommandKind, event.Actor, event.Target, event.Reason,
		event.IdempotencyKey, event.Result, event.BeforeState, event.AfterState, unixMillis(event.CreatedAt)); err != nil {
		return AuditEvent{}, fmt.Errorf("add audit event: %w", err)
	}
	return event, nil
}

// ListAuditEvents returns audit events for a Mission in chronological order.
func (s *Store) ListAuditEvents(ctx context.Context, missionID string) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, mission_id, command_kind, actor, target, reason, idempotency_key, result, before_state, after_state, created_at
		FROM audit_events WHERE mission_id = ? ORDER BY created_at, id`, missionID)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.MissionID, &e.CommandKind, &e.Actor, &e.Target, &e.Reason,
			&e.IdempotencyKey, &e.Result, &e.BeforeState, &e.AfterState, &createdAt); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		e.CreatedAt = fromUnixMillis(createdAt)
		events = append(events, e)
	}
	return events, rows.Err()
}

// FindAuditEventByIdempotencyKey checks if a Command with the given idempotency
// key was already applied for a Mission. Returns the found event and true when
// a match exists; returns false (not an error) when no match is found.
func (s *Store) FindAuditEventByIdempotencyKey(ctx context.Context, missionID, key string) (AuditEvent, bool, error) {
	var e AuditEvent
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, mission_id, command_kind, actor, target, reason, idempotency_key, result, before_state, after_state, created_at
		FROM audit_events WHERE mission_id = ? AND idempotency_key = ? ORDER BY created_at LIMIT 1`,
		missionID, key).
		Scan(&e.ID, &e.MissionID, &e.CommandKind, &e.Actor, &e.Target, &e.Reason,
			&e.IdempotencyKey, &e.Result, &e.BeforeState, &e.AfterState, &createdAt)
	if err == sql.ErrNoRows {
		return AuditEvent{}, false, nil
	}
	if err != nil {
		return AuditEvent{}, false, fmt.Errorf("find audit by idempotency key: %w", err)
	}
	e.CreatedAt = fromUnixMillis(createdAt)
	return e, true, nil
}
