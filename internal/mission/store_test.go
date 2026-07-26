package mission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestStoreCreateAndGetMission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	created, err := store.CreateMission(ctx, CreateMissionInput{
		Goal:               " 实现跨包导入 ",
		AcceptanceContract: "go test ./...",
		BudgetCents:        5000,
		PolicyJSON:         `{"max_concurrency":2}`,
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}
	if created.ID == "" || created.Status != MissionDraft {
		t.Fatalf("created Mission = %+v", created)
	}
	if created.Goal != "实现跨包导入" {
		t.Fatalf("Goal = %q, want trimmed goal", created.Goal)
	}

	got, err := store.GetMission(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMission: %v", err)
	}
	if got.ID != created.ID ||
		got.Goal != created.Goal ||
		got.AcceptanceContract != "go test ./..." ||
		got.BudgetCents != 5000 ||
		got.PolicyJSON != `{"max_concurrency":2}` ||
		got.CurrentPlanVersion != 0 ||
		got.Status != MissionDraft ||
		!got.CreatedAt.Equal(created.CreatedAt) ||
		!got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("GetMission = %+v, want %+v", got, created)
	}
}

func TestStoreCreateMissionPreservesGoalOnlyCallers(t *testing.T) {
	store := newTestStore(t)

	got, err := store.CreateMission(context.Background(), CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}
	if got.AcceptanceContract != "" {
		t.Fatalf("AcceptanceContract = %q, want empty", got.AcceptanceContract)
	}
	if got.PolicyJSON != `{}` {
		t.Fatalf("PolicyJSON = %q, want {}", got.PolicyJSON)
	}
}

func TestStoreCreateMissionRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input CreateMissionInput
	}{
		{name: "blank goal", input: CreateMissionInput{Goal: " \t\n "}},
		{name: "negative budget", input: CreateMissionInput{Goal: "ship", BudgetCents: -1}},
		{name: "malformed policy", input: CreateMissionInput{Goal: "ship", PolicyJSON: `{`}},
		{name: "policy array", input: CreateMissionInput{Goal: "ship", PolicyJSON: `[]`}},
		{name: "policy null", input: CreateMissionInput{Goal: "ship", PolicyJSON: `null`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newTestStore(t).CreateMission(context.Background(), tt.input); err == nil {
				t.Fatal("CreateMission succeeded, want validation error")
			}
		})
	}
}

func TestStoreGetMissionWrapsNotFoundWithID(t *testing.T) {
	store := newTestStore(t)
	const missingID = "missing-mission"

	_, err := store.GetMission(context.Background(), missingID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMission error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), missingID) {
		t.Fatalf("GetMission error = %q, want requested ID", err)
	}
}

func TestStoreListMissionsHasStableOrderingAndDefaultLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const insertMission = `
		INSERT INTO missions (
			id, goal, status, current_plan_version, acceptance_contract,
			budget_cents, policy_json, created_at, updated_at
		) VALUES (?, ?, ?, 0, '', 0, '{}', ?, ?)`
	for _, row := range []struct {
		id        string
		updatedAt int64
	}{
		{id: "mission-c", updatedAt: 1000},
		{id: "mission-b", updatedAt: 2000},
		{id: "mission-a", updatedAt: 2000},
	} {
		if _, err := store.db.ExecContext(ctx, insertMission, row.id, row.id, MissionDraft, int64(500), row.updatedAt); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	got, err := store.ListMissions(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(ListMissions) = %d, want 3", len(got))
	}
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	wantIDs := []string{"mission-a", "mission-b", "mission-c"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("ListMissions IDs = %v, want %v", gotIDs, wantIDs)
		}
	}

	limited, err := store.ListMissions(ctx, 2)
	if err != nil {
		t.Fatalf("ListMissions limited: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "mission-a" || limited[1].ID != "mission-b" {
		t.Fatalf("limited ListMissions = %+v", limited)
	}
}

func TestNewStoreMigratesExistingMissionSchemaIdempotently(t *testing.T) {
	db := openSharedMemoryDB(t)
	createLegacyMissionSchema(t, db)
	const (
		legacyID        = "legacy-mission"
		legacyCreatedAt = int64(1700000000000)
	)
	if _, err := db.Exec(`
		INSERT INTO missions (id, goal, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		legacyID, "preserve me", MissionRunning, legacyCreatedAt, legacyCreatedAt); err != nil {
		t.Fatalf("insert legacy Mission: %v", err)
	}

	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("first NewStore: %v", err)
	}
	if _, err := NewStore(db); err != nil {
		t.Fatalf("second NewStore: %v", err)
	}

	got, err := store.GetMission(context.Background(), legacyID)
	if err != nil {
		t.Fatalf("GetMission legacy row: %v", err)
	}
	if got.Goal != "preserve me" ||
		got.Status != MissionRunning ||
		got.CurrentPlanVersion != 0 ||
		got.AcceptanceContract != "" ||
		got.BudgetCents != 0 ||
		got.PolicyJSON != `{}` ||
		got.CreatedAt.UnixMilli() != legacyCreatedAt {
		t.Fatalf("migrated Mission = %+v", got)
	}

	for table, columns := range map[string][]string{
		"missions":      {"current_plan_version", "acceptance_contract", "budget_cents", "policy_json"},
		"tasks":         {"plan_version", "contract", "tool_scope_json", "budget_cents", "acceptance_json"},
		"task_attempts": {"lease_id"},
		"evidence":      {"verifier_attempt_id"},
	} {
		for _, column := range columns {
			if !tableHasColumn(t, db, table, column) {
				t.Errorf("%s.%s missing after migration", table, column)
			}
		}
	}
	for _, table := range []string{"mission_plan_versions", "mission_events", "mission_change_requests", "mission_commands"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s count = %d, want 1", table, count)
		}
	}
}

func TestStoreCreateMissionWritesCreatedEventAtomically(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	fixedNow := time.Date(2026, 7, 26, 8, 9, 10, 123000000, time.UTC)
	store.now = func() time.Time { return fixedNow }

	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "audit creation"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}
	var (
		eventMissionID string
		eventType      string
		payload        []byte
		createdAt      int64
	)
	if err := store.db.QueryRowContext(ctx, `
		SELECT mission_id, type, payload, created_at
		FROM mission_events WHERE mission_id = ?`, mission.ID).
		Scan(&eventMissionID, &eventType, &payload, &createdAt); err != nil {
		t.Fatalf("query mission.created Event: %v", err)
	}
	if eventMissionID != mission.ID || eventType != "mission.created" || len(payload) == 0 || createdAt != fixedNow.UnixMilli() {
		t.Fatalf("created Event = mission %q, type %q, payload %q, created_at %d",
			eventMissionID, eventType, payload, createdAt)
	}

	if _, err := store.db.ExecContext(ctx, `DROP TABLE mission_events`); err != nil {
		t.Fatalf("drop mission_events: %v", err)
	}
	if _, err := store.CreateMission(ctx, CreateMissionInput{Goal: "must roll back"}); err == nil {
		t.Fatal("CreateMission succeeded without event table")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM missions WHERE goal = 'must roll back'`).Scan(&count); err != nil {
		t.Fatalf("count rolled-back Mission: %v", err)
	}
	if count != 0 {
		t.Fatalf("rolled-back Mission count = %d, want 0", count)
	}
}

func TestStoreCompletingDependencyQueuesBlockedTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateTask(ctx, CreateTaskInput{MissionID: mission.ID, Title: "write specification"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, CreateTaskInput{
		MissionID: mission.ID,
		Title:     "implement feature",
		DependsOn: []string{first.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != TaskBlocked {
		t.Fatalf("initial status = %q, want %q", second.Status, TaskBlocked)
	}

	for _, status := range []TaskStatus{TaskLeased, TaskRunning, TaskVerifying, TaskSucceeded} {
		if _, err := store.TransitionTask(ctx, first.ID, status); err != nil {
			t.Fatalf("transition to %q: %v", status, err)
		}
	}

	got, err := store.GetTask(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskQueued {
		t.Fatalf("dependent status = %q, want %q", got.Status, TaskQueued)
	}
}

func TestStoreRejectsDirectTaskSuccess(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, CreateTaskInput{MissionID: mission.ID, Title: "implement feature"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.TransitionTask(ctx, task.ID, TaskSucceeded); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("TransitionTask error = %v, want ErrInvalidTransition", err)
	}
}

func TestStoreEvidenceIsContentAddressedAndAppendOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	mission, err := store.CreateMission(ctx, CreateMissionInput{Goal: "ship a feature"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, CreateTaskInput{MissionID: mission.ID, Title: "verify feature"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(ctx, task.ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.AddEvidence(ctx, CreateEvidenceInput{
		MissionID: mission.ID,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		Kind:      "go_test",
		Content:   []byte("ok\tgithub.com/harness9/internal/mission"),
		Passed:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SHA256 == "" {
		t.Fatal("evidence SHA256 is empty")
	}
	if evidence.AttemptID != attempt.ID {
		t.Fatalf("attempt ID = %q, want %q", evidence.AttemptID, attempt.ID)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE evidence SET content = ? WHERE id = ?`, []byte("tampered"), evidence.ID); err == nil {
		t.Fatal("expected immutable evidence update to fail")
	}
	if _, err := store.AddEvidence(ctx, CreateEvidenceInput{
		MissionID: mission.ID,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		Kind:      "go_test",
		Content:   []byte("ok\tgithub.com/harness9/internal/mission"),
		Passed:    true,
	}); err != nil {
		t.Fatalf("adding duplicate evidence: %v", err)
	}
	got, err := store.ListEvidence(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(got))
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "mission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func openSharedMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:mission-%s?mode=memory&cache=shared", newID())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createLegacyMissionSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE missions (
			id         TEXT PRIMARY KEY,
			goal       TEXT NOT NULL,
			status     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatalf("create legacy Mission schema: %v", err)
	}
}

func tableHasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			columnTyp string
			notNull   int
			defaultV  any
			primary   int
		)
		if err := rows.Scan(&cid, &name, &columnTyp, &notNull, &defaultV, &primary); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return false
}
