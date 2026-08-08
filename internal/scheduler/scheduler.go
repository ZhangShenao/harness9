package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/harness9/internal/logfmt"
	"github.com/harness9/internal/mission"
)

// SchedulerConfig configures a Scheduler.
type SchedulerConfig struct {
	Store             *mission.Store
	Dispatchers       *RoutingDispatcher
	GlobalConcurrency int
	TickInterval      time.Duration
}

// Scheduler is the deterministic, LLM-free dispatch loop.
type Scheduler struct {
	store             *mission.Store
	dispatchers       *RoutingDispatcher
	globalConcurrency int
	tickInterval      time.Duration
}

// NewScheduler creates a Scheduler.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	if cfg.GlobalConcurrency <= 0 {
		cfg.GlobalConcurrency = 2
	}
	return &Scheduler{
		store:             cfg.Store,
		dispatchers:       cfg.Dispatchers,
		globalConcurrency: cfg.GlobalConcurrency,
		tickInterval:      cfg.TickInterval,
	}
}

// Run starts the dispatch loop. Blocking -- runs until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.tickInterval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick performs one dispatch cycle: find schedulable tasks, check limits, dispatch.
func (s *Scheduler) Tick(ctx context.Context) {
	tasks, err := s.store.ListSchedulableTasks(ctx)
	if err != nil {
		log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("list schedulable: %v", err)))
		return
	}
	if len(tasks) == 0 {
		return
	}
	counts, err := s.store.ActiveTaskCounts(ctx)
	if err != nil {
		log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("active counts: %v", err)))
		return
	}
	for _, task := range tasks {
		if !s.canDispatch(counts, task.MissionID) {
			continue
		}
		if err := s.dispatchOne(ctx, task); err != nil {
			log.Print(logfmt.FormatMsg("scheduler", fmt.Sprintf("dispatch task %s: %v", task.ID, err)))
			continue
		}
		counts["__global__"]++
		counts[task.MissionID]++
	}
}

func (s *Scheduler) canDispatch(counts map[string]int, missionID string) bool {
	if counts["__global__"] >= s.globalConcurrency {
		return false
	}
	return true
}

func (s *Scheduler) dispatchOne(ctx context.Context, task mission.Task) error {
	if err := s.store.MarkMissionRunning(ctx, task.MissionID); err != nil {
		return fmt.Errorf("mark mission running: %w", err)
	}
	attempt, err := s.store.StartAttempt(ctx, task.ID, "worker")
	if err != nil {
		return fmt.Errorf("start attempt: %w", err)
	}
	if _, err := s.store.TransitionTask(ctx, task.ID, mission.TaskLeased); err != nil {
		return fmt.Errorf("transition to leased: %w", err)
	}
	if _, err := s.store.TransitionTask(ctx, task.ID, mission.TaskRunning); err != nil {
		return fmt.Errorf("transition to running: %w", err)
	}

	go func() {
		result, err := s.dispatchers.Dispatch(ctx, task, attempt)
		if err != nil {
			s.store.MarkAttemptFinished(ctx, attempt.ID, "failed", err.Error())
			s.store.TransitionTask(ctx, task.ID, mission.TaskFailed)
			return
		}
		s.handleResult(ctx, task, attempt, result)
	}()
	return nil
}

func (s *Scheduler) handleResult(ctx context.Context, task mission.Task, attempt mission.TaskAttempt, result Result) {
	s.store.MarkAttemptFinished(ctx, attempt.ID, result.Status, result.ExitReason)
	if result.Artifact != nil {
		s.store.AddArtifact(ctx, *result.Artifact)
	}
	switch result.Status {
	case "succeeded":
		s.store.TransitionTask(ctx, task.ID, mission.TaskVerifying)
		s.store.TransitionTask(ctx, task.ID, mission.TaskSucceeded)
		s.store.TryCompleteMission(ctx, task.MissionID)
	case "failed":
		s.store.TransitionTask(ctx, task.ID, mission.TaskFailed)
	case "indeterminate":
		s.store.TransitionTask(ctx, task.ID, mission.TaskIndeterminate)
	}
}

// Reconcile checks for attempts that were running when the process died
// and marks them indeterminate (never blindly retries).
func (s *Scheduler) Reconcile(ctx context.Context) error {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT a.id, a.task_id
		FROM task_attempts a
		WHERE a.status = 'running' AND a.finished_at IS NULL`)
	if err != nil {
		return fmt.Errorf("find interrupted attempts: %w", err)
	}
	defer rows.Close()
	type interrupted struct{ attemptID, taskID string }
	var items []interrupted
	for rows.Next() {
		var item interrupted
		if err := rows.Scan(&item.attemptID, &item.taskID); err != nil {
			return fmt.Errorf("scan interrupted attempt: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate interrupted attempts: %w", err)
	}
	for _, item := range items {
		s.store.MarkAttemptFinished(ctx, item.attemptID, "indeterminate", "process restart")
		s.store.TransitionTask(ctx, item.taskID, mission.TaskIndeterminate)
	}
	return nil
}
