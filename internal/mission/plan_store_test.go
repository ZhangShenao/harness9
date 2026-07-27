package mission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
)

func TestDraftCanBeEditedButApprovedVersionCannot(t *testing.T) {
	store, mission := newStoreWithMission(t)
	ctx := context.Background()

	draft, err := store.CreateDraftPlan(ctx, mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateDraftPlan(ctx, mission.ID, draft.Version, revisedPlanInput(), "user:zsa"); err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(ctx, store, mission.ID, draft.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateDraftPlan(ctx, mission.ID, draft.Version, samplePlanInput(), "user:zsa"); err == nil {
		t.Fatal("已批准 Plan 不能被原地编辑")
	}
}

func TestCreatePlanChangeRequestKeepsCurrentVersion(t *testing.T) {
	store, mission := newApprovedRunningMission(t)
	ctx := context.Background()
	before, err := store.GetMission(ctx, mission.ID)
	if err != nil {
		t.Fatal(err)
	}

	request, err := store.createPlanChangeRequest(
		ctx,
		mission.ID,
		before.CurrentPlanVersion,
		revisedPlanInput(),
		"缺少迁移文档",
		"coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.GetMission(ctx, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != ChangeRequestPending || after.CurrentPlanVersion != before.CurrentPlanVersion {
		t.Fatal("请求不得静默切换 Plan")
	}
}

func TestCreateDraftPlanRejectsUnknownAndCyclicGraphs(t *testing.T) {
	tests := []struct {
		name string
		plan PlanInput
	}{
		{
			name: "unknown dependency",
			plan: PlanInput{Tasks: []TaskInput{{
				ClientID:     "code",
				Title:        "Implement",
				Contract:     "tests pass",
				Dependencies: []string{"missing"},
			}}},
		},
		{
			name: "cycle",
			plan: PlanInput{Tasks: []TaskInput{
				{ClientID: "a", Title: "A", Contract: "A done", Dependencies: []string{"b"}},
				{ClientID: "b", Title: "B", Contract: "B done", Dependencies: []string{"a"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, mission := newStoreWithMission(t)
			if _, err := store.CreateDraftPlan(
				context.Background(),
				mission.ID,
				tt.plan,
				"coordinator",
			); err == nil {
				t.Fatal("CreateDraftPlan succeeded for invalid graph")
			}

			var planCount int
			if err := store.db.QueryRow(
				`SELECT COUNT(*) FROM mission_plan_versions WHERE mission_id = ?`,
				mission.ID,
			).Scan(&planCount); err != nil {
				t.Fatal(err)
			}
			if planCount != 0 {
				t.Fatalf("persisted Plan count = %d, want 0", planCount)
			}
		})
	}
}

func TestCreateDraftPlanAllocatesUniqueVersions(t *testing.T) {
	store, mission := newStoreWithMission(t)
	ctx := context.Background()

	first, err := store.CreateDraftPlan(ctx, mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateDraftPlan(ctx, mission.ID, revisedPlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || second.Version != 2 {
		t.Fatalf("Plan versions = %d, %d; want 1, 2", first.Version, second.Version)
	}

	_, err = store.db.ExecContext(ctx, `
		INSERT INTO mission_plan_versions (
			id, mission_id, version, status, tasks_json, created_at, updated_at
		) VALUES ('duplicate', ?, ?, ?, '{}', 0, 0)`,
		mission.ID,
		first.Version,
		PlanDraft,
	)
	if err == nil {
		t.Fatal("duplicate Mission Plan version was accepted")
	}
}

func TestUpdateDraftPlanRejectsWrongMission(t *testing.T) {
	store, mission := newStoreWithMission(t)
	draft, err := store.CreateDraftPlan(
		context.Background(),
		mission.ID,
		samplePlanInput(),
		"coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateMission(
		context.Background(),
		CreateMissionInput{Goal: "unrelated mission"},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.UpdateDraftPlan(
		context.Background(),
		other.ID,
		draft.Version,
		revisedPlanInput(),
		"user:zsa",
	)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateDraftPlan error = %v, want ErrNotFound", err)
	}
}

func TestUpdateDraftPlanWritesEventAndReturnsDetachedOrderedGraph(t *testing.T) {
	store, mission := newStoreWithMission(t)
	ctx := context.Background()
	draft, err := store.CreateDraftPlan(ctx, mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.UpdateDraftPlan(
		ctx,
		mission.ID,
		draft.Version,
		revisedPlanInput(),
		"user:zsa",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Tasks) != 3 {
		t.Fatalf("updated Task count = %d, want 3", len(updated.Tasks))
	}
	gotOrder := []string{
		updated.Tasks[0].ClientID,
		updated.Tasks[1].ClientID,
		updated.Tasks[2].ClientID,
	}
	wantOrder := []string{"spec", "code", "docs"}
	for index := range wantOrder {
		if gotOrder[index] != wantOrder[index] {
			t.Fatalf("Task order = %v, want %v", gotOrder, wantOrder)
		}
	}
	if len(updated.Tasks[1].Dependencies) != 2 ||
		updated.Tasks[1].Dependencies[0] != "spec" ||
		updated.Tasks[1].Dependencies[1] != "docs" {
		t.Fatalf("resolved dependencies = %v, want [spec docs]", updated.Tasks[1].Dependencies)
	}

	var payload []byte
	if err := store.db.QueryRowContext(ctx, `
		SELECT payload
		FROM mission_events
		WHERE mission_id = ? AND type = 'plan.draft_updated'`,
		mission.ID,
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Actor   string `json:"actor"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Actor != "user:zsa" || event.Version != draft.Version {
		t.Fatalf("draft update Event = %+v", event)
	}

	updated.Tasks[0].Title = "mutated"
	updated.Tasks[1].Dependencies[0] = "mutated"
	readAgain, err := store.GetPlan(ctx, mission.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	if readAgain.Tasks[0].Title != "Revise spec" ||
		readAgain.Tasks[1].Dependencies[0] != "spec" {
		t.Fatalf("persisted Plan was mutated through return slices: %+v", readAgain.Tasks)
	}
}

func TestApprovedPlanReadbackAndTaskReadiness(t *testing.T) {
	store, mission := newStoreWithMission(t)
	ctx := context.Background()
	draft, err := store.CreateDraftPlan(ctx, mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(ctx, store, mission.ID, draft.Version); err != nil {
		t.Fatal(err)
	}

	plan, err := store.GetPlan(ctx, mission.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != PlanApproved || plan.ApprovedAt == nil {
		t.Fatalf("approved Plan = %+v", plan)
	}
	gotMission, err := store.GetMission(ctx, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotMission.Status != MissionReady || gotMission.CurrentPlanVersion != draft.Version {
		t.Fatalf("Mission after approval = %+v", gotMission)
	}
	tasks, err := store.ListTasks(ctx, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	statusByClientID := make(map[string]TaskStatus, len(tasks))
	for _, task := range tasks {
		statusByClientID[task.ClientID] = task.Status
	}
	if statusByClientID["spec"] != TaskQueued || statusByClientID["code"] != TaskBlocked {
		t.Fatalf("approved Task statuses = %v", statusByClientID)
	}

	versions, err := store.ListPlanVersions(ctx, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Status != PlanApproved ||
		len(versions[0].Tasks) != 2 {
		t.Fatalf("ListPlanVersions = %+v", versions)
	}
}

func TestPlanTaskIdentityMigrationPreservesLegacyRows(t *testing.T) {
	db := openSharedMemoryDB(t)
	createLegacyMissionSchema(t, db)
	if _, err := db.Exec(`
		CREATE TABLE tasks (
			id         TEXT PRIMARY KEY,
			mission_id TEXT NOT NULL,
			title      TEXT NOT NULL,
			status     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			FOREIGN KEY (mission_id) REFERENCES missions(id) ON DELETE CASCADE
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO missions (id, goal, status, created_at, updated_at)
		VALUES ('legacy', 'legacy', ?, 1, 1)`,
		MissionDraft,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (id, mission_id, title, status, created_at, updated_at)
		VALUES ('legacy-task', 'legacy', 'legacy', ?, 1, 1)`,
		TaskQueued,
	); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTask(context.Background(), "legacy-task")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "" || got.Position != 0 {
		t.Fatalf("legacy Task graph fields = client %q, position %d", got.ClientID, got.Position)
	}

	const insertPlanTask = `
		INSERT INTO tasks (
			id, mission_id, title, status, plan_version,
			client_id, position, created_at, updated_at
		) VALUES (?, 'legacy', 'new', ?, 1, ?, 0, 1, 1)`
	if _, err := db.Exec(insertPlanTask, "new-1", TaskBlocked, "client-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertPlanTask, "new-2", TaskBlocked, "client-a"); err == nil {
		t.Fatal("partial unique Plan Task identity accepted a duplicate")
	}
	if _, err := db.Exec(insertPlanTask, "legacy-empty-1", TaskBlocked, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertPlanTask, "legacy-empty-2", TaskBlocked, ""); err != nil {
		t.Fatalf("empty legacy client IDs should be exempt from partial uniqueness: %v", err)
	}
}

func TestPendingChangeRequestRetainsCompleteProposedGraph(t *testing.T) {
	store, mission := newApprovedRunningMission(t)
	ctx := context.Background()
	proposal := revisedPlanInput()

	created, err := store.createPlanChangeRequest(
		ctx,
		mission.ID,
		mission.CurrentPlanVersion,
		proposal,
		"missing migration docs",
		"coordinator",
	)
	if err != nil {
		t.Fatal(err)
	}
	proposal.Tasks[0].Title = "mutated after create"
	proposal.Tasks[2].Dependencies[0] = "mutated"

	got, err := store.GetPlanChangeRequest(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseVersion != mission.CurrentPlanVersion ||
		got.Status != ChangeRequestPending ||
		got.Reason != "missing migration docs" ||
		len(got.ProposedPlan.Tasks) != 3 {
		t.Fatalf("Plan Change Request = %+v", got)
	}
	if got.ProposedPlan.Tasks[0].Title != "Migration docs" ||
		got.ProposedPlan.Tasks[2].Dependencies[0] != "spec" {
		t.Fatalf("proposed graph was not retained: %+v", got.ProposedPlan.Tasks)
	}
}

func TestPlanMutationsReturnCachedResultWhenCommitCancelsContext(t *testing.T) {
	t.Run("create draft", func(t *testing.T) {
		store, cancelOnCommit := newCommitCancelStore(t)
		mission, err := store.CreateMission(
			context.Background(),
			CreateMissionInput{Goal: "create plan"},
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelOnCommit(cancel)

		plan, err := store.CreateDraftPlan(ctx, mission.ID, samplePlanInput(), "coordinator")
		if err != nil {
			t.Fatalf("CreateDraftPlan returned an error after durable commit: %v", err)
		}
		if ctx.Err() != context.Canceled || plan.Version != 1 {
			t.Fatalf("CreateDraftPlan result = %+v, context error = %v", plan, ctx.Err())
		}
	})

	t.Run("update draft", func(t *testing.T) {
		store, cancelOnCommit := newCommitCancelStore(t)
		mission, err := store.CreateMission(
			context.Background(),
			CreateMissionInput{Goal: "update plan"},
		)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := store.CreateDraftPlan(
			context.Background(),
			mission.ID,
			samplePlanInput(),
			"coordinator",
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelOnCommit(cancel)

		plan, err := store.UpdateDraftPlan(
			ctx,
			mission.ID,
			draft.Version,
			revisedPlanInput(),
			"user:zsa",
		)
		if err != nil {
			t.Fatalf("UpdateDraftPlan returned an error after durable commit: %v", err)
		}
		if ctx.Err() != context.Canceled || len(plan.Tasks) != 3 {
			t.Fatalf("UpdateDraftPlan result = %+v, context error = %v", plan, ctx.Err())
		}
	})

	t.Run("create change request", func(t *testing.T) {
		store, cancelOnCommit := newCommitCancelStore(t)
		mission, err := store.CreateMission(
			context.Background(),
			CreateMissionInput{Goal: "request plan change"},
		)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := store.CreateDraftPlan(
			context.Background(),
			mission.ID,
			samplePlanInput(),
			"coordinator",
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := markPlanApprovedForTest(
			context.Background(),
			store,
			mission.ID,
			draft.Version,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(
			`UPDATE missions SET status = ? WHERE id = ?`,
			MissionRunning,
			mission.ID,
		); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelOnCommit(cancel)

		request, err := store.createPlanChangeRequest(
			ctx,
			mission.ID,
			draft.Version,
			revisedPlanInput(),
			"missing migration docs",
			"coordinator",
		)
		if err != nil {
			t.Fatalf("createPlanChangeRequest returned an error after durable commit: %v", err)
		}
		if ctx.Err() != context.Canceled || request.Status != ChangeRequestPending {
			t.Fatalf("createPlanChangeRequest result = %+v, context error = %v", request, ctx.Err())
		}
	})
}

func newCommitCancelStore(t *testing.T) (*Store, func(context.CancelFunc)) {
	t.Helper()
	var (
		cancelMu sync.Mutex
		cancel   context.CancelFunc
	)
	driver := &sqlite.Driver{}
	driver.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		hooker, ok := conn.(sqlite.HookRegisterer)
		if !ok {
			return errors.New("sqlite connection does not support commit hooks")
		}
		hooker.RegisterCommitHook(func() int32 {
			cancelMu.Lock()
			cancelFunc := cancel
			cancel = nil
			cancelMu.Unlock()
			if cancelFunc != nil {
				cancelFunc()
			}
			return 0
		})
		return nil
	})
	driverName := "sqlite-plan-commit-cancel-" + newID()
	sql.Register(driverName, driver)
	db, err := sql.Open(driverName, filepath.Join(t.TempDir(), "mission.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store, func(cancelFunc context.CancelFunc) {
		cancelMu.Lock()
		defer cancelMu.Unlock()
		cancel = cancelFunc
	}
}

func newStoreWithMission(t *testing.T) (*Store, Mission) {
	t.Helper()
	store := newTestStore(t)
	mission, err := store.CreateMission(context.Background(), CreateMissionInput{Goal: "ship M2"})
	if err != nil {
		t.Fatal(err)
	}
	return store, mission
}

func newApprovedRunningMission(t *testing.T) (*Store, Mission) {
	t.Helper()
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, samplePlanInput(), "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if err := markPlanApprovedForTest(context.Background(), store, mission.ID, plan.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(
		context.Background(),
		`UPDATE missions SET status = ? WHERE id = ?`,
		MissionRunning,
		mission.ID,
	); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetMission(context.Background(), mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, got
}

func markPlanApprovedForTest(ctx context.Context, store *Store, missionID string, version int) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := store.approvePlanTx(tx, missionID, version, "user:zsa", ""); err != nil {
		return err
	}
	return tx.Commit()
}

func samplePlanInput() PlanInput {
	return PlanInput{Tasks: []TaskInput{
		{ClientID: "spec", Position: 1, Title: "Write spec", Contract: "spec is reviewed"},
		{ClientID: "code", Position: 2, Title: "Implement", Contract: "tests pass", Dependencies: []string{"spec"}},
	}}
}

func revisedPlanInput() PlanInput {
	return PlanInput{Tasks: []TaskInput{
		{ClientID: "docs", Position: 3, Title: "Migration docs", Contract: "docs are complete"},
		{ClientID: "spec", Position: 1, Title: "Revise spec", Contract: "spec is reviewed"},
		{ClientID: "code", Position: 2, Title: "Implement safely", Contract: "tests pass", Dependencies: []string{"spec", "docs"}},
	}}
}

func TestNormalizePlanInputDefaultsContractKindToImplementation(t *testing.T) {
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, PlanInput{Tasks: []TaskInput{
		{ClientID: "a", Position: 1, Title: "A", Contract: "do A"},
	}}, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].ContractKind != ContractImplementation {
		t.Fatalf("ContractKind = %q, want %q", plan.Tasks[0].ContractKind, ContractImplementation)
	}
}

func TestCreateDraftPlanPersistsExplicitContractKind(t *testing.T) {
	store, mission := newStoreWithMission(t)
	plan, err := store.CreateDraftPlan(context.Background(), mission.ID, PlanInput{Tasks: []TaskInput{
		{ClientID: "a", Position: 1, Title: "A", Contract: "do A", ContractKind: ContractVerification},
	}}, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].ContractKind != ContractVerification {
		t.Fatalf("ContractKind = %q, want %q", plan.Tasks[0].ContractKind, ContractVerification)
	}
}
