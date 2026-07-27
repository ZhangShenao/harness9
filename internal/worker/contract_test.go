package worker

import "testing"

func TestImplementationContractIsValid(t *testing.T) {
	if err := ImplementationContract.Validate(); err != nil {
		t.Fatalf("ImplementationContract.Validate(): %v", err)
	}
	if len(ImplementationContract.Tools) == 0 {
		t.Fatal("ImplementationContract.Tools is empty, want an explicit allowlist")
	}
	for _, forbidden := range []string{"task"} {
		for _, tool := range ImplementationContract.Tools {
			if tool == forbidden {
				t.Fatalf("ImplementationContract.Tools contains forbidden tool %q", forbidden)
			}
		}
	}
}

func TestParseResultSuccess(t *testing.T) {
	text := "some narration\n\nTASK_RESULT: SUCCESS\nCOMMIT: abc123def456\n"
	result, err := ParseResult(text)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if !result.Success || result.Commit != "abc123def456" {
		t.Fatalf("result = %+v, want Success=true Commit=abc123def456", result)
	}
}

func TestParseResultFailure(t *testing.T) {
	text := "TASK_RESULT: FAILED\nREASON: tests still failing after 3 attempts\n"
	result, err := ParseResult(text)
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if result.Success {
		t.Fatalf("result.Success = true, want false")
	}
	if result.Reason != "tests still failing after 3 attempts" {
		t.Fatalf("result.Reason = %q, want the failure reason", result.Reason)
	}
}

func TestParseResultRejectsMissingMarker(t *testing.T) {
	if _, err := ParseResult("I did some work but forgot the marker"); err == nil {
		t.Fatal("ParseResult on text without TASK_RESULT = nil error, want an error")
	}
}

func TestParseResultRejectsSuccessWithoutCommit(t *testing.T) {
	if _, err := ParseResult("TASK_RESULT: SUCCESS\n"); err == nil {
		t.Fatal("ParseResult on SUCCESS without COMMIT = nil error, want an error")
	}
}
