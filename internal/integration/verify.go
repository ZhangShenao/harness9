package integration

import (
	"fmt"
	"os/exec"
)

// captureDiff returns the full combined diff between baseSHA and HEAD inside
// worktreePath, for storage as the Integration Task's Artifact. If git
// itself fails, the error text is captured as the artifact content instead
// of losing the failure silently.
func captureDiff(worktreePath, baseSHA string) string {
	cmd := exec.Command("git", "diff", baseSHA, "HEAD")
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("git diff %s HEAD failed: %v\n%s", baseSHA, err, out)
	}
	return string(out)
}
