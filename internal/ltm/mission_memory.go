package ltm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// AddScoped adds a memory entry with a specific scope (project/user/mission/agent).
func (s *Store) AddScoped(ctx context.Context, e Entry) (Entry, error) {
	if e.Scope == "" {
		e.Scope = "project"
	}
	e.Signature = Signature(e.Content)
	e.ID = newID()
	now := s.now()
	e.CreatedAt = now
	e.UpdatedAt = now
	e.LastUsedAt = now
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("add scoped begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO long_term_memories
		(id, title, content, category, importance, signature, created_at, updated_at, last_used_at, use_count, ttl_days, disabled, tags, scope, scope_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, ?, ?, ?)`,
		e.ID, e.Title, e.Content, string(e.Category), e.Importance, e.Signature,
		now.Unix(), now.Unix(), now.Unix(), nullTTL(e.TTLDays), marshalTags(e.Tags), e.Scope, e.ScopeRef); err != nil {
		return Entry{}, fmt.Errorf("add scoped entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories_fts (id, title, content) VALUES (?, ?, ?)`, e.ID, e.Title, e.Content); err != nil {
		return Entry{}, fmt.Errorf("add scoped fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, fmt.Errorf("add scoped commit: %w", err)
	}
	return e, nil
}

// ListByScope returns entries matching a scope and optional scope_ref.
func (s *Store) ListByScope(ctx context.Context, scope, scopeRef string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 30
	}
	query := `SELECT id, title, content, category, importance, signature, created_at, updated_at, last_used_at, use_count, ttl_days, disabled, tags, scope, scope_ref FROM long_term_memories WHERE disabled = 0 AND scope = ?`
	args := []any{scope}
	if scopeRef != "" {
		query += ` AND scope_ref = ?`
		args = append(args, scopeRef)
	}
	query += ` ORDER BY importance DESC, created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list by scope: %w", err)
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		e, err := scanScopedEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *e)
	}
	return entries, rows.Err()
}

// PromoteMissionToProject moves mission-scoped entries to project scope.
func (s *Store) PromoteMissionToProject(ctx context.Context, missionID string) (int, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE long_term_memories SET scope = 'project', scope_ref = '', updated_at = ? WHERE scope = 'mission' AND scope_ref = ?`,
		now.Unix(), missionID)
	if err != nil {
		return 0, fmt.Errorf("promote mission memory: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ArchiveMission marks mission-scoped entries as disabled (not promoted).
func (s *Store) ArchiveMission(ctx context.Context, missionID string) (int, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE long_term_memories SET disabled = 1, signature = NULL, updated_at = ? WHERE scope = 'mission' AND scope_ref = ?`,
		now.Unix(), missionID)
	if err != nil {
		return 0, fmt.Errorf("archive mission memory: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanScopedEntry(sc scanner) (*Entry, error) {
	var e Entry
	var category, signature, tags sql.NullString
	var createdAt, updatedAt int64
	var lastUsed, ttlDays sql.NullInt64
	var disabled int
	var scope, scopeRef sql.NullString
	if err := sc.Scan(&e.ID, &e.Title, &e.Content, &category, &e.Importance, &signature,
		&createdAt, &updatedAt, &lastUsed, &e.UseCount, &ttlDays, &disabled, &tags, &scope, &scopeRef); err != nil {
		return nil, err
	}
	e.Category = Category(category.String)
	e.Signature = signature.String
	e.CreatedAt = time.Unix(createdAt, 0)
	e.UpdatedAt = time.Unix(updatedAt, 0)
	if lastUsed.Valid {
		e.LastUsedAt = time.Unix(lastUsed.Int64, 0)
	}
	if ttlDays.Valid {
		e.TTLDays = int(ttlDays.Int64)
	}
	e.Disabled = disabled != 0
	if tags.String != "" {
		_ = json.Unmarshal([]byte(tags.String), &e.Tags)
	}
	if scope.Valid {
		e.Scope = scope.String
	}
	if scopeRef.Valid {
		e.ScopeRef = scopeRef.String
	}
	return &e, nil
}
