package verifier

import (
	"fmt"
	"os/exec"
	"strings"
)

// verificationReport is the outcome of one independent re-verification pass.
type verificationReport struct {
	passed bool
	output string
}

// runVerificationChecks independently re-runs the checks every implementation
// Task Contract already runs itself (build, vet, test) inside workDir,
// stopping at the first failing step. No LLM is involved: an independent
// deterministic rerun is a signal a self-reported success cannot fake, which
// is exactly what Mission's "Verifier must not verify its own artifact"
// invariant requires.
func runVerificationChecks(workDir string) verificationReport {
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
			return verificationReport{passed: false, output: output.String()}
		}
	}
	return verificationReport{passed: true, output: output.String()}
}
