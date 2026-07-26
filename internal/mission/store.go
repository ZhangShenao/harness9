package mission

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
`

const missionSchemaSQL = `
CREATE TABLE IF NOT EXISTS mission_plan_versions (
    id          TEXT PRIMARY KEY,
    mission_id  TEXT NOT NULL,
    version     INTEGER NOT NULL,
    status      TEXT NOT NULL,
    tasks_json  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    approved_at INTEGER,
    UNIQUE (mission_id, version),
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS mission_events (
    id         TEXT PRIMARY KEY,
    mission_id TEXT NOT NULL,
    task_id    TEXT NOT NULL DEFAULT '',
    attempt_id TEXT NOT NULL DEFAULT '',
    type       TEXT NOT NULL,
    payload    BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS mission_change_requests (
    id                       TEXT PRIMARY KEY,
    mission_id               TEXT NOT NULL,
    trigger_attempt_id       TEXT NOT NULL DEFAULT '',
    reason                   TEXT NOT NULL,
    impacted_task_ids_json   TEXT NOT NULL,
    permission_change        TEXT NOT NULL DEFAULT '',
    budget_change            TEXT NOT NULL DEFAULT '',
    proposed_plan_json       TEXT NOT NULL,
    status                   TEXT NOT NULL,
    resolution_reason        TEXT NOT NULL DEFAULT '',
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    resolved_at              INTEGER,
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS mission_commands (
    id              TEXT PRIMARY KEY,
    mission_id      TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    type            TEXT NOT NULL,
    actor           TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    payload         BLOB NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE (mission_id, idempotency_key),
    FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_missions_status_updated
ON missions(status, updated_at DESC, id);

CREATE INDEX IF NOT EXISTS idx_tasks_mission_plan_status
ON tasks(mission_id, plan_version, status);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_mission_plan_client
ON tasks(mission_id, plan_version, client_id)
WHERE client_id IS NOT NULL AND client_id != '';

CREATE INDEX IF NOT EXISTS idx_task_attempts_task_status
ON task_attempts(task_id, status);

CREATE INDEX IF NOT EXISTS idx_workspace_leases_task_status
ON workspace_leases(task_id, status);

CREATE INDEX IF NOT EXISTS idx_mission_events_mission_id
ON mission_events(mission_id, id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_mission_active_lease_per_task
ON workspace_leases(task_id)
WHERE status IN ('active', 'releasing');

CREATE TRIGGER IF NOT EXISTS prevent_mission_event_update
BEFORE UPDATE ON mission_events
BEGIN
    SELECT RAISE(ABORT, 'mission event is immutable');
END;

CREATE TRIGGER IF NOT EXISTS prevent_mission_event_delete
BEFORE DELETE ON mission_events
BEGIN
    SELECT RAISE(ABORT, 'mission event is immutable');
END;
`

var schemaMigrations = []struct {
	table      string
	column     string
	definition string
}{
	{table: "missions", column: "current_plan_version", definition: "INTEGER NOT NULL DEFAULT 0"},
	{table: "missions", column: "acceptance_contract", definition: "TEXT NOT NULL DEFAULT ''"},
	{table: "missions", column: "budget_cents", definition: "INTEGER NOT NULL DEFAULT 0"},
	{table: "missions", column: "policy_json", definition: "TEXT NOT NULL DEFAULT '{}'"},
	{table: "tasks", column: "plan_version", definition: "INTEGER NOT NULL DEFAULT 0"},
	{table: "tasks", column: "client_id", definition: "TEXT"},
	{table: "tasks", column: "position", definition: "INTEGER NOT NULL DEFAULT 0"},
	{table: "tasks", column: "contract", definition: "TEXT NOT NULL DEFAULT ''"},
	{table: "tasks", column: "tool_scope_json", definition: "TEXT NOT NULL DEFAULT '{}'"},
	{table: "tasks", column: "budget_cents", definition: "INTEGER NOT NULL DEFAULT 0"},
	{table: "tasks", column: "acceptance_json", definition: "TEXT NOT NULL DEFAULT '{}'"},
	{table: "task_attempts", column: "lease_id", definition: "TEXT"},
	{table: "workspace_leases", column: "branch", definition: "TEXT NOT NULL DEFAULT ''"},
	{table: "workspace_leases", column: "sandbox_id", definition: "TEXT NOT NULL DEFAULT ''"},
	{table: "evidence", column: "verifier_attempt_id", definition: "TEXT"},
}

// Store is the SQLite-backed source of truth for Mission Control state.
type Store struct {
	db  *sql.DB
	now func() time.Time
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
	if err := migrateMissionSchema(db); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

func migrateMissionSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin mission schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schemaSQL); err != nil {
		return fmt.Errorf("initialize mission schema: %w", err)
	}
	for _, migration := range schemaMigrations {
		if err := addColumnIfMissing(tx, migration.table, migration.column, migration.definition); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(missionSchemaSQL); err != nil {
		return fmt.Errorf("initialize extended mission schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mission schema migration: %w", err)
	}
	return nil
}

func addColumnIfMissing(tx *sql.Tx, table, column, definition string) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count); err != nil {
		return fmt.Errorf("inspect %s.%s schema: %w", table, column, err)
	}
	if count > 0 {
		return nil
	}
	query := fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN "%s" %s`, table, column, definition)
	if _, err := tx.Exec(query); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// CreateMission creates a draft Mission with the supplied user goal.
func (s *Store) CreateMission(ctx context.Context, in CreateMissionInput) (Mission, error) {
	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		return Mission{}, fmt.Errorf("mission goal is required")
	}
	if in.BudgetCents < 0 {
		return Mission{}, fmt.Errorf("mission budget cannot be negative")
	}
	policyJSON, err := normalizeJSONObject(in.PolicyJSON)
	if err != nil {
		return Mission{}, fmt.Errorf("mission policy: %w", err)
	}
	now := s.currentTime()
	mission := Mission{
		ID:                 newID(),
		Goal:               goal,
		AcceptanceContract: strings.TrimSpace(in.AcceptanceContract),
		BudgetCents:        in.BudgetCents,
		PolicyJSON:         policyJSON,
		CurrentPlanVersion: 0,
		Status:             MissionDraft,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Mission{}, fmt.Errorf("begin mission transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO missions (
			id, goal, status, current_plan_version, acceptance_contract,
			budget_cents, policy_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mission.ID, mission.Goal, mission.Status, mission.CurrentPlanVersion,
		mission.AcceptanceContract, mission.BudgetCents, mission.PolicyJSON,
		unixMillis(now), unixMillis(now)); err != nil {
		return Mission{}, fmt.Errorf("insert mission: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"actor":  "system",
		"status": mission.Status,
	})
	if err != nil {
		return Mission{}, fmt.Errorf("marshal mission.created event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: mission.ID,
		Type:      "mission.created",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return Mission{}, err
	}
	if err := tx.Commit(); err != nil {
		return Mission{}, fmt.Errorf("commit mission: %w", err)
	}
	return mission, nil
}

// GetMission returns one durable Mission by ID.
func (s *Store) GetMission(ctx context.Context, id string) (Mission, error) {
	mission, err := scanMission(s.db.QueryRowContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions WHERE id = ?`, id))
	if err == ErrNotFound {
		return Mission{}, fmt.Errorf("mission %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Mission{}, err
	}
	return mission, nil
}

// ListMissions returns the most recently updated Missions in stable order.
func (s *Store) ListMissions(ctx context.Context, limit int) ([]Mission, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, goal, acceptance_contract, budget_cents, policy_json,
		       current_plan_version, status, created_at, updated_at
		FROM missions
		ORDER BY updated_at DESC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list missions: %w", err)
	}
	defer rows.Close()
	missions := make([]Mission, 0)
	for rows.Next() {
		mission, err := scanMission(rows)
		if err != nil {
			return nil, err
		}
		missions = append(missions, mission)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate missions: %w", err)
	}
	return missions, nil
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

	now := s.currentTime()
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
		`SELECT id, mission_id, title, COALESCE(client_id, ''), position, contract,
		        status, created_at, updated_at
		 FROM tasks WHERE id = ?`, id))
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
		`SELECT id, mission_id, title, COALESCE(client_id, ''), position, contract,
		        status, created_at, updated_at
		 FROM tasks WHERE mission_id = ? ORDER BY created_at, id`, missionID)
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
// Compatibility covers this method signature and named composite literals of
// returned records; positional composite literals are not a supported contract.
func (s *Store) StartAttempt(ctx context.Context, taskID, worker string) (TaskAttempt, error) {
	taskID = strings.TrimSpace(taskID)
	worker = strings.TrimSpace(worker)
	if taskID == "" {
		return TaskAttempt{}, fmt.Errorf("attempt task ID is required")
	}
	if worker == "" {
		return TaskAttempt{}, fmt.Errorf("attempt worker is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TaskAttempt{}, fmt.Errorf("begin task attempt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	task, err := scanTask(tx.QueryRowContext(ctx, `
		SELECT id, mission_id, title, COALESCE(client_id, ''), position, contract,
		       status, created_at, updated_at
		FROM tasks WHERE id = ?`, taskID))
	if err != nil {
		return TaskAttempt{}, err
	}
	if task.Status != TaskQueued && task.Status != TaskLeased && task.Status != TaskRunning {
		return TaskAttempt{}, fmt.Errorf(
			"%w: task %s cannot start an attempt from %s",
			ErrInvalidTransition,
			taskID,
			task.Status,
		)
	}
	now := s.currentTime()
	var leaseID string
	var expiresAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT id, expires_at
		FROM workspace_leases
		WHERE task_id = ? AND status IN ('active', 'releasing')
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, taskID,
	).Scan(&leaseID, &expiresAt)
	switch {
	case err == nil && expiresAt <= unixMillis(now):
		return TaskAttempt{}, fmt.Errorf("%w: task %s lease is expired", ErrConflict, taskID)
	case err == sql.ErrNoRows && task.Status != TaskQueued:
		return TaskAttempt{}, fmt.Errorf("%w: task %s has no active lease", ErrConflict, taskID)
	case err != nil && err != sql.ErrNoRows:
		return TaskAttempt{}, fmt.Errorf("read task lease: %w", err)
	}
	if task.Status == TaskLeased {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks
			SET status = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			TaskRunning,
			unixMillis(now),
			taskID,
			TaskLeased,
		); err != nil {
			return TaskAttempt{}, fmt.Errorf("mark task running: %w", err)
		}
	}
	attempt := TaskAttempt{
		ID:        newID(),
		TaskID:    task.ID,
		LeaseID:   leaseID,
		Worker:    worker,
		Status:    AttemptRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_attempts (
			id, task_id, lease_id, worker, status, created_at, updated_at
		) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?)`,
		attempt.ID,
		attempt.TaskID,
		attempt.LeaseID,
		attempt.Worker,
		attempt.Status,
		unixMillis(now),
		unixMillis(now),
	); err != nil {
		return TaskAttempt{}, fmt.Errorf("create task attempt: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"worker":   attempt.Worker,
		"lease_id": attempt.LeaseID,
		"status":   attempt.Status,
	})
	if err != nil {
		return TaskAttempt{}, fmt.Errorf("marshal attempt.started event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: task.MissionID,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		Type:      "attempt.started",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return TaskAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskAttempt{}, fmt.Errorf("commit task attempt: %w", err)
	}
	return attempt, nil
}

// AddArtifact records Worker output once and returns an existing matching artifact on retry.
// Compatibility covers this signature and named CreateArtifactInput literals.
func (s *Store) AddArtifact(ctx context.Context, in CreateArtifactInput) (Artifact, error) {
	if len(in.Content) == 0 {
		return Artifact{}, fmt.Errorf("artifact content is required")
	}
	if strings.TrimSpace(in.Kind) == "" {
		return Artifact{}, fmt.Errorf("artifact kind is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, fmt.Errorf("begin artifact transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateAttemptWith(ctx, tx, in.MissionID, in.TaskID, in.AttemptID); err != nil {
		return Artifact{}, err
	}
	digestValue := digest(in.Content)
	if artifact, ok, err := findArtifactWith(
		ctx,
		tx,
		in.AttemptID,
		in.Kind,
		digestValue,
	); err != nil {
		return Artifact{}, err
	} else if ok {
		return artifact, nil
	}
	now := s.currentTime()
	artifact := Artifact{ID: newID(), MissionID: in.MissionID, TaskID: in.TaskID, AttemptID: in.AttemptID, Kind: in.Kind, Content: append([]byte(nil), in.Content...), SHA256: digestValue, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts (id, mission_id, task_id, attempt_id, kind, content, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID, artifact.MissionID, artifact.TaskID, artifact.AttemptID, artifact.Kind, artifact.Content, artifact.SHA256, unixMillis(now)); err != nil {
		return Artifact{}, fmt.Errorf("add artifact: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"artifact_id": artifact.ID,
		"kind":        artifact.Kind,
		"sha256":      artifact.SHA256,
	})
	if err != nil {
		return Artifact{}, fmt.Errorf("marshal artifact.created event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: artifact.MissionID,
		TaskID:    artifact.TaskID,
		AttemptID: artifact.AttemptID,
		Type:      "artifact.created",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return Artifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, fmt.Errorf("commit artifact: %w", err)
	}
	return artifact, nil
}

// AddEvidence records verifier output once and returns a matching record on retry.
// Compatibility covers this signature and named CreateEvidenceInput literals;
// optional metadata fields may be appended without supporting positional literals.
func (s *Store) AddEvidence(ctx context.Context, in CreateEvidenceInput) (Evidence, error) {
	if len(in.Content) == 0 {
		return Evidence{}, fmt.Errorf("evidence content is required")
	}
	if strings.TrimSpace(in.Kind) == "" {
		return Evidence{}, fmt.Errorf("evidence kind is required")
	}
	in.VerifierAttemptID = strings.TrimSpace(in.VerifierAttemptID)
	if in.VerifierAttemptID != "" && in.VerifierAttemptID == in.AttemptID {
		return Evidence{}, fmt.Errorf(
			"%w: evidence producer and verifier attempts must differ",
			ErrConflict,
		)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Evidence{}, fmt.Errorf("begin evidence transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateAttemptWith(ctx, tx, in.MissionID, in.TaskID, in.AttemptID); err != nil {
		return Evidence{}, err
	}
	if in.VerifierAttemptID != "" {
		if err := validateAttemptWith(
			ctx,
			tx,
			in.MissionID,
			in.TaskID,
			in.VerifierAttemptID,
		); err != nil {
			return Evidence{}, fmt.Errorf("validate verifier attempt: %w", err)
		}
	}
	digestValue := digest(in.Content)
	if evidence, ok, err := findEvidenceWith(ctx, tx, in.AttemptID, in.Kind, digestValue); err != nil {
		return Evidence{}, err
	} else if ok {
		return evidence, nil
	}
	now := s.currentTime()
	evidence := Evidence{ID: newID(), MissionID: in.MissionID, TaskID: in.TaskID, AttemptID: in.AttemptID, VerifierAttemptID: in.VerifierAttemptID, Kind: in.Kind, Content: append([]byte(nil), in.Content...), SHA256: digestValue, Passed: in.Passed, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evidence (
			id, mission_id, task_id, attempt_id, verifier_attempt_id,
			kind, content, sha256, passed, created_at
		) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		evidence.ID,
		evidence.MissionID,
		evidence.TaskID,
		evidence.AttemptID,
		evidence.VerifierAttemptID,
		evidence.Kind,
		evidence.Content,
		evidence.SHA256,
		evidence.Passed,
		unixMillis(now),
	); err != nil {
		return Evidence{}, fmt.Errorf("add evidence: %w", err)
	}
	payload, err := json.Marshal(map[string]any{
		"evidence_id":         evidence.ID,
		"verifier_attempt_id": evidence.VerifierAttemptID,
		"kind":                evidence.Kind,
		"sha256":              evidence.SHA256,
		"passed":              evidence.Passed,
	})
	if err != nil {
		return Evidence{}, fmt.Errorf("marshal evidence.created event: %w", err)
	}
	if err := insertEvent(ctx, tx, Event{
		ID:        newID(),
		MissionID: evidence.MissionID,
		TaskID:    evidence.TaskID,
		AttemptID: evidence.AttemptID,
		Type:      "evidence.created",
		Payload:   payload,
		CreatedAt: now,
	}); err != nil {
		return Evidence{}, err
	}
	if err := tx.Commit(); err != nil {
		return Evidence{}, fmt.Errorf("commit evidence: %w", err)
	}
	return evidence, nil
}

// ListEvidence returns a Task's verifier output in creation order.
func (s *Store) ListEvidence(ctx context.Context, taskID string) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, mission_id, task_id, attempt_id,
		       COALESCE(verifier_attempt_id, ''), kind, content, sha256, passed, created_at
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
		if err := rows.Scan(
			&item.ID,
			&item.MissionID,
			&item.TaskID,
			&item.AttemptID,
			&item.VerifierAttemptID,
			&item.Kind,
			&item.Content,
			&item.SHA256,
			&passed,
			&createdAt,
		); err != nil {
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
		`SELECT id, mission_id, title, COALESCE(client_id, ''), position, contract,
		        status, created_at, updated_at
		 FROM tasks WHERE id = ?`, id))
	if err != nil {
		return Task{}, err
	}
	if !validTaskTransition(current.Status, next) {
		return Task{}, fmt.Errorf("%w: task %s cannot move from %s to %s", ErrInvalidTransition, id, current.Status, next)
	}
	now := s.currentTime()
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

func scanMission(row rowScanner) (Mission, error) {
	var mission Mission
	var createdAt, updatedAt int64
	if err := row.Scan(
		&mission.ID,
		&mission.Goal,
		&mission.AcceptanceContract,
		&mission.BudgetCents,
		&mission.PolicyJSON,
		&mission.CurrentPlanVersion,
		&mission.Status,
		&createdAt,
		&updatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return Mission{}, ErrNotFound
		}
		return Mission{}, fmt.Errorf("scan mission: %w", err)
	}
	mission.CreatedAt = fromUnixMillis(createdAt)
	mission.UpdatedAt = fromUnixMillis(updatedAt)
	return mission, nil
}

func scanTask(row rowScanner) (Task, error) {
	var task Task
	var createdAt, updatedAt int64
	if err := row.Scan(
		&task.ID,
		&task.MissionID,
		&task.Title,
		&task.ClientID,
		&task.Position,
		&task.Contract,
		&task.Status,
		&createdAt,
		&updatedAt,
	); err != nil {
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

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateAttemptWith(ctx context.Context, q queryRower, missionID, taskID, attemptID string) error {
	var count int
	if err := q.QueryRowContext(ctx, `
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

func findEvidenceWith(ctx context.Context, q queryRower, attemptID, kind, digestValue string) (Evidence, bool, error) {
	var evidence Evidence
	var passed int
	var createdAt int64
	err := q.QueryRowContext(ctx, `
		SELECT id, mission_id, task_id, attempt_id,
		       COALESCE(verifier_attempt_id, ''), kind, content, sha256, passed, created_at
		FROM evidence WHERE attempt_id = ? AND kind = ? AND sha256 = ?`, attemptID, kind, digestValue).
		Scan(
			&evidence.ID,
			&evidence.MissionID,
			&evidence.TaskID,
			&evidence.AttemptID,
			&evidence.VerifierAttemptID,
			&evidence.Kind,
			&evidence.Content,
			&evidence.SHA256,
			&passed,
			&createdAt,
		)
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

func findArtifactWith(
	ctx context.Context,
	q queryRower,
	attemptID string,
	kind string,
	digestValue string,
) (Artifact, bool, error) {
	var artifact Artifact
	var createdAt int64
	err := q.QueryRowContext(ctx, `
		SELECT id, mission_id, task_id, attempt_id, kind, content, sha256, created_at
		FROM artifacts
		WHERE attempt_id = ? AND kind = ? AND sha256 = ?`,
		attemptID,
		kind,
		digestValue,
	).Scan(
		&artifact.ID,
		&artifact.MissionID,
		&artifact.TaskID,
		&artifact.AttemptID,
		&artifact.Kind,
		&artifact.Content,
		&artifact.SHA256,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, fmt.Errorf("find artifact: %w", err)
	}
	artifact.CreatedAt = fromUnixMillis(createdAt)
	return artifact, true, nil
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

func insertEvent(ctx context.Context, tx *sql.Tx, event Event) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mission_events (
			id, mission_id, task_id, attempt_id, type, payload, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		event.MissionID,
		event.TaskID,
		event.AttemptID,
		event.Type,
		event.Payload,
		unixMillis(event.CreatedAt),
	); err != nil {
		return fmt.Errorf("insert mission event: %w", err)
	}
	return nil
}

func normalizeJSONObject(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return `{}`, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &object); err != nil || object == nil {
		return "", fmt.Errorf("must be a JSON object")
	}
	return value, nil
}

func (s *Store) currentTime() time.Time {
	now := s.now
	if now == nil {
		now = time.Now
	}
	return fromUnixMillis(unixMillis(now()))
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
