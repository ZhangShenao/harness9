// Package coordinator provides the LLM-driven Coordinator agent that
// decomposes complex tasks into Plans, monitors execution progress, and
// proposes Plan Change Requests when needed. The Coordinator can only
// submit structured proposals -- it has no scheduling privileges.
package coordinator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harness9/internal/mission"
)

// Coordinator drives the Mission lifecycle: decompose, monitor, adapt.
type Coordinator struct {
	store *mission.Store
}

// NewCoordinator creates a Coordinator backed by the given Store.
func NewCoordinator(store *mission.Store) *Coordinator {
	return &Coordinator{store: store}
}

// DecomposeGoal takes a user goal and creates a Mission with a draft Plan.
// For S4, this creates a single-Task implementation Plan (LLM decomposition
// is a future enhancement -- the structure is in place to extend).
func (c *Coordinator) DecomposeGoal(ctx context.Context, goal string) (mission.Mission, mission.Plan, error) {
	m, err := c.store.CreateMission(ctx, mission.CreateMissionInput{Goal: goal})
	if err != nil {
		return mission.Mission{}, mission.Plan{}, fmt.Errorf("create mission: %w", err)
	}

	// Create a single implementation task as draft plan
	taskInput := mission.TaskInput{
		Kind:       mission.ContractImplementation,
		Goal:       goal,
		Acceptance: []string{"go build ./... passes", "go test ./... passes"},
		Budget:     mission.Budget{MaxTurns: 50, MaxTokens: 100000},
		MaxRetries: 2,
	}
	tasksJSON, _ := json.Marshal([]mission.TaskInput{taskInput})
	plan, err := c.store.CreatePlan(ctx, m.ID, string(tasksJSON))
	if err != nil {
		return m, mission.Plan{}, fmt.Errorf("create plan: %w", err)
	}

	// Transition mission to planning
	updated, err := c.store.TransitionMission(ctx, m.ID, mission.MissionPlanning)
	if err != nil {
		return m, plan, fmt.Errorf("transition to planning: %w", err)
	}
	m = updated

	return m, plan, nil
}

// CreateTaskFromPlan creates actual Task records from an approved Plan's task definitions.
func (c *Coordinator) CreateTaskFromPlan(ctx context.Context, planID string) error {
	plan, err := c.store.GetPlan(ctx, planID)
	if err != nil {
		return fmt.Errorf("get plan: %w", err)
	}

	var tasks []mission.TaskInput
	if err := json.Unmarshal([]byte(plan.TasksJSON), &tasks); err != nil {
		return fmt.Errorf("unmarshal plan tasks: %w", err)
	}

	for _, ti := range tasks {
		input := mission.CreateTaskInput{
			MissionID: plan.MissionID,
			Title:     ti.Goal,
		}
		task, err := c.store.CreateTask(ctx, input)
		if err != nil {
			return fmt.Errorf("create task: %w", err)
		}
		// Set contract kind and input
		inputJSON, _ := json.Marshal(ti)
		c.store.DB().ExecContext(ctx,
			`UPDATE tasks SET contract_kind = ?, input_json = ?, max_retries = ? WHERE id = ?`,
			ti.Kind, string(inputJSON), ti.MaxRetries, task.ID)
	}

	return nil
}

// Monitor checks a Mission's progress and returns a status summary.
func (c *Coordinator) Monitor(ctx context.Context, missionID string) (string, error) {
	m, err := c.store.GetMission(ctx, missionID)
	if err != nil {
		return "", err
	}
	tasks, err := c.store.ListTasks(ctx, missionID)
	if err != nil {
		return "", err
	}
	succeeded := 0
	for _, t := range tasks {
		if t.Status == mission.TaskSucceeded {
			succeeded++
		}
	}
	return fmt.Sprintf("Mission %s: status=%s, tasks=%d, succeeded=%d", missionID[:8], m.Status, len(tasks), succeeded), nil
}
