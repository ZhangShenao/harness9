package verifier

import (
	"fmt"
	"os/exec"
	"strings"
)

// VerificationReport is the outcome of one independent re-verification pass.
type VerificationReport struct {
	Passed bool
	Output string
}

// RunVerificationChecks independently re-runs the checks every implementation
// Task Contract already runs itself (build, vet, test) inside workDir,
// stopping at the first failing step. No LLM is involved: an independent
// deterministic rerun is a signal a self-reported success cannot fake, which
// is exactly what Mission's "Verifier must not verify its own artifact"
// invariant requires. Exported so internal/integration can reuse it for
// joint verification after merging multiple Tasks' branches together.
func RunVerificationChecks(workDir string) VerificationReport {
	var output strings.Builder
	for _, args := range [][]string{
		{"build", "./..."},
		{"vet", "./..."},
		{"test", "./...", "-timeout", "5m"},
	} {
		cmd := exec.Command("go", args...)
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		fmt.Fprintf(&output, "$ go %s\n%s\n", strings.Join(args, " "), out)
		if err != nil {
			return VerificationReport{Passed: false, Output: output.String()}
		}
	}
	return VerificationReport{Passed: true, Output: output.String()}
}
