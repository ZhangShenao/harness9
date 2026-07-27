// Package scheduler assigns queued Mission Control Tasks to Workers without
// using an LLM for any safety-critical decision. It only reads and writes
// durable state through internal/mission.Store, so a restarted Scheduler
// resumes from the same source of truth instead of private memory.
package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/harness9/internal/mission"
)

// Dispatcher launches one Worker Attempt for a schedulable Task. Implementations
// must leave the Task in mission.TaskQueued when Dispatch returns an error, so
// the Scheduler retries the Task on a later Tick instead of losing it.
type Dispatcher interface {
	Dispatch(ctx context.Context, task mission.Task) error
}

// Scheduler assigns queued Tasks to a Dispatcher within global and per-Mission
// concurrency limits. It holds no unpersisted state: every Tick re-reads the
// durable Store, so it is safe to construct a fresh Scheduler after a restart.
type Scheduler struct {
	store                *mission.Store
	dispatcher           Dispatcher
	maxGlobalConcurrency int
}

// Option configures a Scheduler constructed by NewScheduler.
type Option func(*Scheduler)

// WithMaxGlobalConcurrency caps how many Tasks may be leased across all
// Missions at once. n must be positive; non-positive values are ignored and
// the default of 4 is kept.
func WithMaxGlobalConcurrency(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.maxGlobalConcurrency = n
		}
	}
}

// NewScheduler creates a Scheduler backed by store and dispatcher.
func NewScheduler(store *mission.Store, dispatcher Dispatcher, opts ...Option) *Scheduler {
	s := &Scheduler{store: store, dispatcher: dispatcher, maxGlobalConcurrency: 4}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Tick performs one non-blocking dispatch pass: it lists schedulable Tasks,
// respects global and per-Mission concurrency limits, transitions each
// Mission from ready to running the first time it dispatches into it, and
// calls Dispatcher.Dispatch for every Task within budget. It returns how many
// Tasks it dispatched and a joined error for Tasks it could not dispatch;
// those Tasks remain queued and are retried on a later Tick. Tick is not safe
// for concurrent use — call it from a single goroutine (see Run).
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	perMissionActive, globalActive, err := s.store.ActiveTaskCounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("count active tasks: %w", err)
	}
	tasks, err := s.store.ListSchedulableTasks(ctx)
	if err != nil {
		return 0, fmt.Errorf("list schedulable tasks: %w", err)
	}

	remainingGlobal := s.maxGlobalConcurrency - globalActive
	policies := make(map[string]mission.Policy)
	dispatched := 0
	var dispatchErrors []error

	for _, task := range tasks {
		if remainingGlobal <= 0 {
			break
		}
		policy, cached := policies[task.MissionID]
		if !cached {
			policy, err = s.prepareMission(ctx, task.MissionID)
			if err != nil {
				dispatchErrors = append(dispatchErrors, fmt.Errorf("task %s: %w", task.ID, err))
				continue
			}
			policies[task.MissionID] = policy
		}
		if perMissionActive[task.MissionID] >= policy.MaxConcurrentTasks {
			continue
		}
		if err := s.dispatcher.Dispatch(ctx, task); err != nil {
			dispatchErrors = append(dispatchErrors, fmt.Errorf("task %s: %w", task.ID, err))
			continue
		}
		perMissionActive[task.MissionID]++
		remainingGlobal--
		dispatched++
	}
	return dispatched, errors.Join(dispatchErrors...)
}

// prepareMission loads a Mission's Policy and, the first time this Tick sees a
// ready Mission, transitions it to running before any Task is dispatched.
func (s *Scheduler) prepareMission(ctx context.Context, missionID string) (mission.Policy, error) {
	m, err := s.store.GetMission(ctx, missionID)
	if err != nil {
		return mission.Policy{}, fmt.Errorf("load mission: %w", err)
	}
	policy, err := mission.ParsePolicy(m.PolicyJSON)
	if err != nil {
		return mission.Policy{}, fmt.Errorf("parse mission policy: %w", err)
	}
	if m.Status == mission.MissionReady {
		if _, err := s.store.MarkMissionRunning(ctx, missionID); err != nil {
			return mission.Policy{}, fmt.Errorf("mark mission running: %w", err)
		}
	}
	return policy, nil
}
