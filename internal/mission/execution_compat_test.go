package mission_test

import (
	"context"

	mission "github.com/harness9/internal/mission"
)

// These declarations intentionally compile outside package mission. They lock
// compatibility to exported method signatures and named composite literals;
// positional composite literals are not part of the compatibility contract.
var (
	_ func(*mission.Store, context.Context, string, string) (mission.TaskAttempt, error)           = (*mission.Store).StartAttempt
	_ func(*mission.Store, context.Context, mission.CreateArtifactInput) (mission.Artifact, error) = (*mission.Store).AddArtifact
	_ func(*mission.Store, context.Context, mission.CreateEvidenceInput) (mission.Evidence, error) = (*mission.Store).AddEvidence
	_                                                                                              = mission.TaskAttempt{
		ID:     "attempt",
		TaskID: "task",
		Worker: "worker",
		Status: mission.AttemptRunning,
	}
	_ = mission.Evidence{
		ID:        "evidence",
		MissionID: "mission",
		TaskID:    "task",
		AttemptID: "producer",
		Kind:      "go_test",
		Content:   []byte("ok"),
		SHA256:    "digest",
		Passed:    true,
	}
	_ = mission.CreateArtifactInput{
		MissionID: "mission",
		TaskID:    "task",
		AttemptID: "attempt",
		Kind:      "report",
		Content:   []byte("result"),
	}
	_ = mission.CreateEvidenceInput{
		MissionID: "mission",
		TaskID:    "task",
		AttemptID: "producer",
		Kind:      "go_test",
		Content:   []byte("ok"),
		Passed:    true,
	}
)
