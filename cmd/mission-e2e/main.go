// Package main implements a manually-invoked harness for M2's frozen
// Feature Mission acceptance run (design doc §9.1 phase 3): it wires the
// real Mission Control Plane (Store + Scheduler + Worker/Verifier/
// Integration Adapters) to a real LLM provider and drives one Mission to
// completion with genuine sub-agent calls. It intentionally lives outside
// `go test` — internal/evals.SetupHermeticEnv exists specifically to keep
// real API calls out of the test suite — since this tool's entire purpose
// is to spend real API budget proving the pipeline built across
// internal/mission, internal/scheduler, internal/worker, internal/verifier,
// and internal/integration works end-to-end against a genuine feature
// rather than a synthetic test fixture.
//
// Usage:
//
//	go run ./cmd/mission-e2e -repo /path/to/harness9 -db /tmp/mission-e2e/mission.db
//	go run ./cmd/mission-e2e -repo /path/to/harness9 -dry-run
//
// Environment variables (via .env in the target repo, or system env):
//
//	OPENAI_API_KEY   LLM Provider API Key (required unless -dry-run)
//	OPENAI_BASE_URL  Custom OpenAI-compatible endpoint (optional)
//	LLM_MODEL        Model name (default: openai/gpt-4o-mini)
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/harness9/internal/env"
	"github.com/harness9/internal/hooks"
	"github.com/harness9/internal/integration"
	"github.com/harness9/internal/mission"
	"github.com/harness9/internal/provider"
	"github.com/harness9/internal/scheduler"
	"github.com/harness9/internal/verifier"
	"github.com/harness9/internal/worker"
)

func main() {
	repoRoot := flag.String("repo", "", "target repository root new Task worktrees are created relative to (required)")
	dbPath := flag.String("db", "", "path to the Mission SQLite database (default: <repo>/.harness9/mission-e2e/mission.db)")
	model := flag.String("model", "", "LLM model name (default: $LLM_MODEL, falling back to openai/gpt-4o-mini)")
	pollInterval := flag.Duration("poll", 5*time.Second, "Scheduler Tick interval")
	timeout := flag.Duration("timeout", 30*time.Minute, "maximum wall-clock time to wait for the Mission to reach a terminal status")
	dryRun := flag.Bool("dry-run", false, "create and approve the Mission and Plan, print Task IDs, then exit without dispatching any Attempt")
	flag.Parse()

	if strings.TrimSpace(*repoRoot) == "" {
		log.Fatal("mission-e2e: -repo is required")
	}
	absRepoRoot, err := filepath.Abs(*repoRoot)
	if err != nil {
		log.Fatalf("mission-e2e: resolve -repo: %v", err)
	}

	if err := env.Load(filepath.Join(absRepoRoot, ".env")); err != nil {
		log.Fatalf("mission-e2e: load .env: %v", err)
	}

	modelName := strings.TrimSpace(*model)
	if modelName == "" {
		modelName = os.Getenv("LLM_MODEL")
	}
	if modelName == "" {
		modelName = "openai/gpt-4o-mini"
	}

	if strings.TrimSpace(*dbPath) == "" {
		*dbPath = filepath.Join(absRepoRoot, ".harness9", "mission-e2e", "mission.db")
	}
	absDBPath, err := filepath.Abs(*dbPath)
	if err != nil {
		log.Fatalf("mission-e2e: resolve -db: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDBPath), 0o755); err != nil {
		log.Fatalf("mission-e2e: create db directory: %v", err)
	}

	dsn := absDBPath + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("mission-e2e: open mission db: %v", err)
	}
	defer db.Close()

	store, err := mission.NewStore(db)
	if err != nil {
		log.Fatalf("mission-e2e: init mission store: %v", err)
	}

	ctx := context.Background()

	m, err := store.CreateMission(ctx, mission.CreateMissionInput{
		Goal:       featureMissionGoal,
		PolicyJSON: featureMissionPolicyJSON,
	})
	if err != nil {
		log.Fatalf("mission-e2e: create mission: %v", err)
	}
	fmt.Printf("mission created: id=%s\n", m.ID)

	plan, err := store.CreateDraftPlan(ctx, m.ID, mission.PlanInput{Tasks: featureMissionTasks()}, "mission-e2e")
	if err != nil {
		log.Fatalf("mission-e2e: create draft plan: %v", err)
	}
	fmt.Printf("plan drafted: version=%d\n", plan.Version)

	svc := mission.NewCommandService(store)
	if _, err := svc.ApprovePlan(ctx, mission.ApprovePlanCommand{
		MissionID: m.ID, Version: plan.Version, Actor: "user:mission-e2e",
		Reason: "frozen feature mission acceptance run", IdempotencyKey: "mission-e2e-approve-" + m.ID,
	}); err != nil {
		log.Fatalf("mission-e2e: approve plan: %v", err)
	}
	fmt.Println("plan approved")

	if _, err := store.MarkMissionRunning(ctx, m.ID); err != nil {
		log.Fatalf("mission-e2e: mark mission running: %v", err)
	}
	fmt.Println("mission marked running")

	tasks, err := store.ListTasks(ctx, m.ID)
	if err != nil {
		log.Fatalf("mission-e2e: list tasks: %v", err)
	}
	fmt.Println("tasks:")
	for _, task := range tasks {
		fmt.Printf("  %-24s id=%-38s kind=%-16s status=%s\n", task.ClientID, task.ID, task.ContractKind, task.Status)
	}

	if *dryRun {
		fmt.Println("dry-run: exiting without dispatching any Attempt")
		return
	}

	if os.Getenv("OPENAI_API_KEY") == "" {
		log.Fatal("mission-e2e: OPENAI_API_KEY is not set (needed to actually dispatch Attempts; use -dry-run to skip this)")
	}

	llmProvider, err := provider.NewOpenAIProvider(modelName)
	if err != nil {
		log.Fatalf("mission-e2e: create provider: %v", err)
	}

	executorCfg := worker.RunnerExecutorConfig{
		ProviderFor: func(callModel string) (provider.LLMProvider, int, error) {
			if callModel == "" {
				return llmProvider, provider.GetModelLimits(modelName).ContextTokens, nil
			}
			p, err := provider.NewOpenAIProvider(callModel)
			if err != nil {
				return nil, 0, err
			}
			return p, provider.GetModelLimits(callModel).ContextTokens, nil
		},
		SharedHooks:     []hooks.ToolHook{hooks.NewDangerHook()},
		DefaultMaxTurns: 60,
		ToolTimeout:     120 * time.Second,
		BaseCtx:         ctx,
	}
	executor := worker.NewRunnerExecutor(executorCfg)

	workerAdapter := worker.NewAdapter(store, absRepoRoot, executor, ctx)
	verifierAdapter := verifier.NewAdapter(store, absRepoRoot, ctx)
	integrationAdapter := integration.NewAdapter(store, absRepoRoot, ctx)

	dispatcher := scheduler.NewRoutingDispatcher(map[string]scheduler.Dispatcher{
		mission.ContractImplementation: workerAdapter,
		mission.ContractVerification:   verifierAdapter,
		mission.ContractIntegration:    integrationAdapter,
	})

	sched := scheduler.NewScheduler(store, dispatcher, scheduler.WithMaxGlobalConcurrency(4))

	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if _, err := sched.RecoverInterrupted(runCtx); err != nil {
		log.Fatalf("mission-e2e: recover interrupted attempts: %v", err)
	}

	fmt.Printf("scheduler running (poll every %s, timeout %s)...\n", *pollInterval, *timeout)
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()

	lastReport := make(map[string]mission.TaskStatus, len(tasks))
	for {
		select {
		case <-runCtx.Done():
			fmt.Println("timed out waiting for mission to reach a terminal status")
			printReport(ctx, store, m.ID)
			os.Exit(1)
		case <-ticker.C:
			dispatched, err := sched.Tick(runCtx)
			if err != nil {
				fmt.Printf("tick error: %v\n", err)
			}
			if dispatched > 0 {
				fmt.Printf("dispatched %d task(s) this tick\n", dispatched)
			}
			reportTaskChanges(ctx, store, m.ID, lastReport)

			got, err := store.GetMission(ctx, m.ID)
			if err != nil {
				log.Fatalf("mission-e2e: get mission: %v", err)
			}
			switch got.Status {
			case mission.MissionSucceeded, mission.MissionFailed, mission.MissionNeedsAttention:
				fmt.Printf("mission reached terminal status: %s\n", got.Status)
				printReport(ctx, store, m.ID)
				if got.Status == mission.MissionSucceeded {
					return
				}
				os.Exit(1)
			}
		}
	}
}

// reportTaskChanges prints a line for every Task whose status changed since
// the last poll, and updates seen in place.
func reportTaskChanges(ctx context.Context, store *mission.Store, missionID string, seen map[string]mission.TaskStatus) {
	tasks, err := store.ListTasks(ctx, missionID)
	if err != nil {
		fmt.Printf("list tasks: %v\n", err)
		return
	}
	for _, task := range tasks {
		if seen[task.ID] != task.Status {
			fmt.Printf("  [%s] %s -> %s\n", task.ClientID, seen[task.ID], task.Status)
			seen[task.ID] = task.Status
		}
	}
}

// printReport prints every Task's final status plus its Evidence, so the
// operator can judge the Mission's real acceptance without needing to query
// the SQLite database by hand.
func printReport(ctx context.Context, store *mission.Store, missionID string) {
	tasks, err := store.ListTasks(ctx, missionID)
	if err != nil {
		fmt.Printf("list tasks: %v\n", err)
		return
	}
	fmt.Println("\n=== final report ===")
	for _, task := range tasks {
		fmt.Printf("\n--- %s (%s, %s) ---\n", task.ClientID, task.ContractKind, task.Status)
		evidence, err := store.ListEvidence(ctx, task.ID)
		if err != nil {
			fmt.Printf("list evidence: %v\n", err)
			continue
		}
		for _, e := range evidence {
			verdict := "FAIL"
			if e.Passed {
				verdict = "PASS"
			}
			content := string(e.Content)
			if len(content) > 2000 {
				content = content[:2000] + "\n... (truncated)"
			}
			fmt.Printf("  evidence[%s] %s:\n%s\n", e.Kind, verdict, content)
		}
	}
}
