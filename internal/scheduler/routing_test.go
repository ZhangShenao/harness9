package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/harness9/internal/mission"
)

type recordingDispatcher struct {
	calls []string
	err   error
}

func (d *recordingDispatcher) Dispatch(ctx context.Context, task mission.Task) error {
	d.calls = append(d.calls, task.ID)
	return d.err
}

func TestRoutingDispatcherRoutesByContractKind(t *testing.T) {
	impl := &recordingDispatcher{}
	verify := &recordingDispatcher{}
	router := NewRoutingDispatcher(map[string]Dispatcher{
		mission.ContractImplementation: impl,
		mission.ContractVerification:   verify,
	})

	if err := router.Dispatch(context.Background(), mission.Task{ID: "t1", ContractKind: mission.ContractImplementation}); err != nil {
		t.Fatalf("Dispatch (implementation): %v", err)
	}
	if err := router.Dispatch(context.Background(), mission.Task{ID: "t2", ContractKind: mission.ContractVerification}); err != nil {
		t.Fatalf("Dispatch (verification): %v", err)
	}
	if len(impl.calls) != 1 || impl.calls[0] != "t1" {
		t.Fatalf("impl.calls = %v, want [t1]", impl.calls)
	}
	if len(verify.calls) != 1 || verify.calls[0] != "t2" {
		t.Fatalf("verify.calls = %v, want [t2]", verify.calls)
	}
}

func TestRoutingDispatcherDefaultsEmptyContractKindToImplementation(t *testing.T) {
	impl := &recordingDispatcher{}
	router := NewRoutingDispatcher(map[string]Dispatcher{mission.ContractImplementation: impl})

	if err := router.Dispatch(context.Background(), mission.Task{ID: "t1", ContractKind: ""}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(impl.calls) != 1 {
		t.Fatalf("impl.calls = %v, want exactly one call", impl.calls)
	}
}

func TestRoutingDispatcherRejectsUnknownContractKind(t *testing.T) {
	router := NewRoutingDispatcher(map[string]Dispatcher{mission.ContractImplementation: &recordingDispatcher{}})
	err := router.Dispatch(context.Background(), mission.Task{ID: "t1", ContractKind: "integration"})
	if err == nil {
		t.Fatal("Dispatch with unregistered contract kind = nil error, want an error")
	}
}

func TestRoutingDispatcherPropagatesSubDispatcherError(t *testing.T) {
	failing := &recordingDispatcher{err: errors.New("boom")}
	router := NewRoutingDispatcher(map[string]Dispatcher{mission.ContractImplementation: failing})
	if err := router.Dispatch(context.Background(), mission.Task{ID: "t1"}); err == nil {
		t.Fatal("Dispatch = nil error, want the sub-dispatcher's error propagated")
	}
}
