package mission

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS missions (
    id         TEXT PRIMARY KEY,
    goal       TEXT NOT NULL,
    status     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id         TEXT PRIMARY KEY,
    mission_id TEXT NOT NULL,
    title      TEXT NOT NULL,
    status     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_tasks_mission_status ON tasks(mission_id, status);

CREATE TABLE IF NOT EXISTS task_dependencies (
    task_id       TEXT NOT NULL,
    dependency_id TEXT NOT NULL,
    PRIMARY KEY (task_id, dependency_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (dependency_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_task_dependencies_dependency ON task_dependencies(dependency_id);

CREATE TABLE IF NOT EXISTS task_attempts (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL,
    worker     TEXT NOT NULL,
    status     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS artifacts (
    id         TEXT PRIMARY KEY,
    mission_id TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    kind       TEXT NOT NULL,
    content    BLOB NOT NULL,
    sha256     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (attempt_id) REFERENCES task_attempts(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS evidence (
    id         TEXT PRIMARY KEY,
    mission_id TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    kind       TEXT NOT NULL,
    content    BLOB NOT NULL,
    sha256     TEXT NOT NULL,
    passed     INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (attempt_id) REFERENCES task_attempts(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS workspace_leases (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL,
    path        TEXT NOT NULL,
    status      TEXT NOT NULL,
    expires_at  INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    released_at INTEGER,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspace_leases_active_task
ON workspace_leases(task_id) WHERE status = 'active';

CREATE UNIQUE INDEX IF NOT EXISTS idx_evidence_attempt_kind_digest
ON evidence(attempt_id, kind, sha256);

CREATE TRIGGER IF NOT EXISTS prevent_evidence_update
BEFORE UPDATE ON evidence
BEGIN
    SELECT RAISE(ABORT, 'evidence is immutable');
END;

CREATE TRIGGER IF NOT EXISTS prevent_artifact_update
BEFORE UPDATE ON artifacts
BEGIN
    SELECT RAISE(ABORT, 'artifact is immutable');
END;

CREATE TABLE IF NOT EXISTS plans (
    id         TEXT PRIMARY KEY,
    mission_id TEXT NOT NULL,
    version    INTEGER NOT NULL,
    status     TEXT NOT NULL,
    tasks_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS plan_versions (
    id         TEXT PRIMARY KEY,
    mission_id TEXT NOT NULL,
    plan_id    TEXT NOT NULL,
    version    INTEGER NOT NULL,
    tasks_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE,
    FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS plan_change_requests (
    id                 TEXT PRIMARY KEY,
    mission_id         TEXT NOT NULL,
    reason             TEXT NOT NULL,
    trigger_attempt_id TEXT,
    affected_tasks     TEXT,
    added_tasks        TEXT,
    proposed_plan_json TEXT NOT NULL,
    status             TEXT NOT NULL,
    reviewed_by        TEXT,
    reviewed_at        INTEGER,
    review_reason      TEXT,
    created_at         INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS policies (
    mission_id TEXT PRIMARY KEY,
    policy_json TEXT NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS audit_events (
    id              TEXT PRIMARY KEY,
    mission_id      TEXT NOT NULL,
    command_kind    TEXT NOT NULL,
    actor           TEXT NOT NULL,
    target          TEXT,
    reason          TEXT,
    idempotency_key TEXT,
    result          TEXT NOT NULL,
    before_state    TEXT,
    after_state     TEXT,
    created_at      INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_audit_events_mission ON audit_events(mission_id, created_at);
CREATE INDEX IF NOT EXISTS idx_plan_change_requests_mission ON plan_change_requests(mission_id, status);
`

// Store is the SQLite-backed source of truth for Mission Control state.
type Store struct {
	db *sql.DB
}

// NewStore initializes Mission Control tables on an existing SQLite connection.
// The caller retains ownership of db and must close it through its own lifecycle.
func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("mission database is required")
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("enable mission foreign keys: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("initialize mission schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("mission migration: %w", err)
	}
	return s, nil
}

// DB returns the underlying database connection for scheduler queries
// that don't have dedicated Store methods yet.
func (s *Store) DB() *sql.DB { return s.db }

// CreateMission creates a draft Mission with the supplied user goal.
func (s *Store) CreateMission(ctx context.Context, in CreateMissionInput) (Mission, error) {
	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		return Mission{}, fmt.Errorf("mission goal is required")
	}
	now := time.Now().UTC()
	mission := Mission{ID: newID(), Goal: goal, Status: MissionDraft, CreatedAt: now, UpdatedAt: now}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO missions (id, goal, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		mission.ID, mission.Goal, mission.Status, unixMillis(now), unixMillis(now)); err != nil {
		return Mission{}, fmt.Errorf("create mission: %w", err)
	}
	return mission, nil
}

// CreateTask persists a Task after validating its Mission and prerequisites.
func (s *Store) CreateTask(ctx context.Context, in CreateTaskInput) (Task, error) {
	if strings.TrimSpace(in.MissionID) == "" {
		return Task{}, fmt.Errorf("task mission ID is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Task{}, fmt.Errorf("task title is required")
	}
	dependsOn := uniqueIDs(in.DependsOn)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("begin task transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := missionExists(ctx, tx, in.MissionID); err != nil {
		return Task{}, err
	}
	for _, dependencyID := range dependsOn {
		if err := dependencyInMission(ctx, tx, in.MissionID, dependencyID); err != nil {
			return Task{}, err
		}
	}

	now := time.Now().UTC()
	status := TaskQueued
	if len(dependsOn) > 0 {
		status = TaskBlocked
	}
	task := Task{ID: newID(), MissionID: in.MissionID, Title: title, Status: status, DependsOn: dependsOn, CreatedAt: now, UpdatedAt: now}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (id, mission_id, title, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		task.ID, task.MissionID, task.Title, task.Status, unixMillis(now), unixMillis(now)); err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	for _, dependencyID := range dependsOn {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_dependencies (task_id, dependency_id) VALUES (?, ?)`, task.ID, dependencyID); err != nil {
			return Task{}, fmt.Errorf("insert task dependency: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("commit task: %w", err)
	}
	return task, nil
}

// GetTask reads a Task and its dependency IDs.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	task, err := scanTask(s.db.QueryRowContext(ctx,
		`SELECT id, mission_id, title, status, created_at, updated_at FROM tasks WHERE id = ?`, id))
	if err != nil {
		return Task{}, err
	}
	dependsOn, err := s.taskDependencies(ctx, id)
	if err != nil {
		return Task{}, err
	}
	task.DependsOn = dependsOn
	return task, nil
}

// ListTasks returns a Mission's Tasks in creation order.
func (s *Store) ListTasks(ctx context.Context, missionID string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mission_id, title, status, created_at, updated_at FROM tasks WHERE mission_id = ? ORDER BY created_at, id`, missionID)
	if err != nil {
		return nil, fmt.Errorf("list mission tasks: %w", err)
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
		return nil, fmt.Errorf("iterate mission tasks: %w", err)
	}
	return tasks, nil
}

// StartAttempt records a Worker attempt for an existing Task.
func (s *Store) StartAttempt(ctx context.Context, taskID, worker string) (TaskAttempt, error) {
	if strings.TrimSpace(worker) == "" {
		return TaskAttempt{}, fmt.Errorf("attempt worker is required")
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return TaskAttempt{}, err
	}
	now := time.Now().UTC()
	attempt := TaskAttempt{
		ID: newID(), TaskID: task.ID, Worker: strings.TrimSpace(worker), Status: "running", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO task_attempts (id, task_id, worker, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		attempt.ID, attempt.TaskID, attempt.Worker, attempt.Status, unixMillis(now), unixMillis(now)); err != nil {
		return TaskAttempt{}, fmt.Errorf("create task attempt: %w", err)
	}
	return attempt, nil
}

// AddArtifact records Worker output once and returns an existing matching artifact on retry.
func (s *Store) AddArtifact(ctx context.Context, in CreateArtifactInput) (Artifact, error) {
	if len(in.Content) == 0 {
		return Artifact{}, fmt.Errorf("artifact content is required")
	}
	if strings.TrimSpace(in.Kind) == "" {
		return Artifact{}, fmt.Errorf("artifact kind is required")
	}
	if err := s.validateAttempt(ctx, in.MissionID, in.TaskID, in.AttemptID); err != nil {
		return Artifact{}, err
	}
	now := time.Now().UTC()
	artifact := Artifact{ID: newID(), MissionID: in.MissionID, TaskID: in.TaskID, AttemptID: in.AttemptID, Kind: in.Kind, Content: append([]byte(nil), in.Content...), SHA256: digest(in.Content), CreatedAt: now}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO artifacts (id, mission_id, task_id, attempt_id, kind, content, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID, artifact.MissionID, artifact.TaskID, artifact.AttemptID, artifact.Kind, artifact.Content, artifact.SHA256, unixMillis(now)); err != nil {
		return Artifact{}, fmt.Errorf("add artifact: %w", err)
	}
	return artifact, nil
}

// AddEvidence records verifier output once and returns a matching record on retry.
func (s *Store) AddEvidence(ctx context.Context, in CreateEvidenceInput) (Evidence, error) {
	if len(in.Content) == 0 {
		return Evidence{}, fmt.Errorf("evidence content is required")
	}
	if strings.TrimSpace(in.Kind) == "" {
		return Evidence{}, fmt.Errorf("evidence kind is required")
	}
	if err := s.validateAttempt(ctx, in.MissionID, in.TaskID, in.AttemptID); err != nil {
		return Evidence{}, err
	}
	digestValue := digest(in.Content)
	if evidence, ok, err := s.findEvidence(ctx, in.AttemptID, in.Kind, digestValue); err != nil {
		return Evidence{}, err
	} else if ok {
		return evidence, nil
	}
	now := time.Now().UTC()
	evidence := Evidence{ID: newID(), MissionID: in.MissionID, TaskID: in.TaskID, AttemptID: in.AttemptID, Kind: in.Kind, Content: append([]byte(nil), in.Content...), SHA256: digestValue, Passed: in.Passed, CreatedAt: now}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO evidence (id, mission_id, task_id, attempt_id, kind, content, sha256, passed, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evidence.ID, evidence.MissionID, evidence.TaskID, evidence.AttemptID, evidence.Kind, evidence.Content, evidence.SHA256, evidence.Passed, unixMillis(now)); err != nil {
		return Evidence{}, fmt.Errorf("add evidence: %w", err)
	}
	return evidence, nil
}

// ListEvidence returns a Task's verifier output in creation order.
func (s *Store) ListEvidence(ctx context.Context, taskID string) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, mission_id, task_id, attempt_id, kind, content, sha256, passed, created_at
		FROM evidence WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	defer rows.Close()
	var evidence []Evidence
	for rows.Next() {
		var item Evidence
		var passed int
		var createdAt int64
		if err := rows.Scan(&item.ID, &item.MissionID, &item.TaskID, &item.AttemptID, &item.Kind, &item.Content, &item.SHA256, &passed, &createdAt); err != nil {
			return nil, fmt.Errorf("scan evidence: %w", err)
		}
		item.Passed = passed != 0
		item.CreatedAt = fromUnixMillis(createdAt)
		evidence = append(evidence, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evidence: %w", err)
	}
	return evidence, nil
}

// TransitionTask validates and applies one Task lifecycle transition.
func (s *Store) TransitionTask(ctx context.Context, id string, next TaskStatus) (Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, fmt.Errorf("begin task transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanTask(tx.QueryRowContext(ctx,
		`SELECT id, mission_id, title, status, created_at, updated_at FROM tasks WHERE id = ?`, id))
	if err != nil {
		return Task{}, err
	}
	if !validTaskTransition(current.Status, next) {
		return Task{}, fmt.Errorf("%w: task %s cannot move from %s to %s", ErrInvalidTransition, id, current.Status, next)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, next, unixMillis(now), id); err != nil {
		return Task{}, fmt.Errorf("update task status: %w", err)
	}
	if next == TaskSucceeded {
		if err := queueReadyDependents(ctx, tx, id, now); err != nil {
			return Task{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("commit task transition: %w", err)
	}
	return s.GetTask(ctx, id)
}

type rowScanner interface {
	Scan(...any) error
}

func scanTask(row rowScanner) (Task, error) {
	var task Task
	var createdAt, updatedAt int64
	if err := row.Scan(&task.ID, &task.MissionID, &task.Title, &task.Status, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return Task{}, ErrNotFound
		}
		return Task{}, fmt.Errorf("scan task: %w", err)
	}
	task.CreatedAt = fromUnixMillis(createdAt)
	task.UpdatedAt = fromUnixMillis(updatedAt)
	return task, nil
}

func (s *Store) taskDependencies(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dependency_id FROM task_dependencies WHERE task_id = ? ORDER BY dependency_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task dependencies: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan task dependency: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task dependencies: %w", err)
	}
	return ids, nil
}

func missionExists(ctx context.Context, tx *sql.Tx, missionID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM missions WHERE id = ?`, missionID).Scan(&count); err != nil {
		return fmt.Errorf("check mission: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func dependencyInMission(ctx context.Context, tx *sql.Tx, missionID, dependencyID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ? AND mission_id = ?`, dependencyID, missionID).Scan(&count); err != nil {
		return fmt.Errorf("check task dependency: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("dependency %s: %w", dependencyID, ErrNotFound)
	}
	return nil
}

func (s *Store) validateAttempt(ctx context.Context, missionID, taskID, attemptID string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM task_attempts attempt
		JOIN tasks task ON task.id = attempt.task_id
		WHERE attempt.id = ? AND attempt.task_id = ? AND task.mission_id = ?`,
		attemptID, taskID, missionID).Scan(&count); err != nil {
		return fmt.Errorf("validate task attempt: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) findEvidence(ctx context.Context, attemptID, kind, digestValue string) (Evidence, bool, error) {
	var evidence Evidence
	var passed int
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, mission_id, task_id, attempt_id, kind, content, sha256, passed, created_at
		FROM evidence WHERE attempt_id = ? AND kind = ? AND sha256 = ?`, attemptID, kind, digestValue).
		Scan(&evidence.ID, &evidence.MissionID, &evidence.TaskID, &evidence.AttemptID, &evidence.Kind, &evidence.Content, &evidence.SHA256, &passed, &createdAt)
	if err == sql.ErrNoRows {
		return Evidence{}, false, nil
	}
	if err != nil {
		return Evidence{}, false, fmt.Errorf("find evidence: %w", err)
	}
	evidence.Passed = passed != 0
	evidence.CreatedAt = fromUnixMillis(createdAt)
	return evidence, true, nil
}

func queueReadyDependents(ctx context.Context, tx *sql.Tx, dependencyID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM task_dependencies WHERE dependency_id = ?`, dependencyID)
	if err != nil {
		return fmt.Errorf("find dependent tasks: %w", err)
	}
	defer rows.Close()
	var dependentIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan dependent task: %w", err)
		}
		dependentIDs = append(dependentIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate dependent tasks: %w", err)
	}
	for _, taskID := range dependentIDs {
		var unmet int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM task_dependencies d
			JOIN tasks dependency ON dependency.id = d.dependency_id
			WHERE d.task_id = ? AND dependency.status != ?`, taskID, TaskSucceeded).Scan(&unmet); err != nil {
			return fmt.Errorf("check unmet dependencies: %w", err)
		}
		if unmet == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, TaskQueued, unixMillis(now), taskID, TaskBlocked); err != nil {
				return fmt.Errorf("queue dependent task: %w", err)
			}
		}
	}
	return nil
}

func uniqueIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func newID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Sprintf("read mission ID entropy: %v", err))
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:])
}

func unixMillis(t time.Time) int64 { return t.UnixMilli() }

func fromUnixMillis(v int64) time.Time { return time.UnixMilli(v).UTC() }

// migrate applies idempotent column additions for enhanced tables.
func (s *Store) migrate(ctx context.Context) error {
	taskCols := []struct{ col, typ string }{
		{"plan_version_id", "TEXT"},
		{"contract_kind", "TEXT DEFAULT 'implementation'"},
		{"input_json", "TEXT"},
		{"budget_json", "TEXT"},
		{"max_retries", "INTEGER DEFAULT 0"},
	}
	for _, c := range taskCols {
		if err := addColumnIfMissing(ctx, s.db, "tasks", c.col, c.typ); err != nil {
			return fmt.Errorf("migrate tasks.%s: %w", c.col, err)
		}
	}
	attemptCols := []struct{ col, typ string }{
		{"lease_id", "TEXT"},
		{"exit_reason", "TEXT"},
		{"started_at", "INTEGER"},
		{"finished_at", "INTEGER"},
	}
	for _, c := range attemptCols {
		if err := addColumnIfMissing(ctx, s.db, "task_attempts", c.col, c.typ); err != nil {
			return fmt.Errorf("migrate task_attempts.%s: %w", c.col, err)
		}
	}
	missionCols := []struct{ col, typ string }{
		{"policy_json", "TEXT"},
		{"acceptance_contract", "TEXT"},
		{"current_plan_version", "TEXT"},
	}
	for _, c := range missionCols {
		if err := addColumnIfMissing(ctx, s.db, "missions", c.col, c.typ); err != nil {
			return fmt.Errorf("migrate missions.%s: %w", c.col, err)
		}
	}
	leaseCols := []struct{ col, typ string }{
		{"branch", "TEXT"},
		{"sandbox_id", "TEXT"},
	}
	for _, c := range leaseCols {
		if err := addColumnIfMissing(ctx, s.db, "workspace_leases", c.col, c.typ); err != nil {
			return fmt.Errorf("migrate workspace_leases.%s: %w", c.col, err)
		}
	}
	return nil
}

func addColumnIfMissing(ctx context.Context, db *sql.DB, table, column, typ string) error {
	exists, err := columnExists(ctx, db, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, typ))
	return err
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, fmt.Errorf("check column %s.%s: %w", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan table_info: %w", err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) columnNames(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}
