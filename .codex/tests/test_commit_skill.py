#!/usr/bin/env python3
from pathlib import Path
import os
import re
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[2]
SKILL = ROOT / ".agents" / "skills" / "commit" / "SKILL.md"


class CommitAuditContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.text = SKILL.read_text(encoding="utf-8")

    def test_audit_directory_is_private_and_cleanup_is_registered(self) -> None:
        self.assertIn("umask 077", self.text)
        self.assertIn('chmod 700 "$AUDIT_DIR"', self.text)
        self.assertIn("cleanup_audit()", self.text)
        self.assertIn("trap cleanup_audit EXIT", self.text)
        self.assertLess(
            self.text.index("trap cleanup_audit EXIT"),
            self.text.index("AUDIT_DIR=$(mktemp -d)"),
            "cleanup must be registered before audit directory creation",
        )

    def test_cleanup_is_verified_for_all_exit_paths(self) -> None:
        self.assertIn('test ! -e "$AUDIT_DIR"', self.text)
        self.assertRegex(
            self.text,
            (
                r"success,\s+refusal,\s+error,\s+cancellation,\s+"
                r"and every stop path"
            ),
        )

    def test_parent_tree_and_nul_path_audits_remain_required(self) -> None:
        required = (
            "git diff --cached --name-only -z --no-renames",
            'test "$(git rev-parse HEAD)" = "$BASE_HEAD"',
            'test "$(git write-tree)" = "$EXPECTED_TREE"',
            'test "$(git rev-parse "$NEW_HEAD^")" = "$BASE_HEAD"',
            'test "$(git rev-parse "$NEW_HEAD^{tree}")" = "$EXPECTED_TREE"',
            'cmp "$AUDIT_DIR/expected-paths" "$AUDIT_DIR/actual-paths"',
        )
        for contract in required:
            with self.subTest(contract=contract):
                self.assertIn(contract, self.text)

    def test_audit_recipe_cleans_success_and_forced_exit(self) -> None:
        match = re.search(
            r"5\. Record the exact candidate commit.*?"
            r"```bash\n(?P<body>.*?)\n   ```",
            self.text,
            flags=re.DOTALL,
        )
        self.assertIsNotNone(match, "audit recipe missing")
        recipe = textwrap.dedent(match.group("body"))

        with tempfile.TemporaryDirectory(
            prefix="harness9-commit-audit-",
            dir="/tmp",
        ) as root:
            repository = Path(root) / "repository"
            audit_parent = Path(root) / "audits"
            repository.mkdir()
            audit_parent.mkdir()
            environment = os.environ.copy()
            environment.update(
                {
                    "TMPDIR": str(audit_parent),
                    "GIT_AUTHOR_NAME": "Audit Test",
                    "GIT_AUTHOR_EMAIL": "audit@example.invalid",
                    "GIT_COMMITTER_NAME": "Audit Test",
                    "GIT_COMMITTER_EMAIL": "audit@example.invalid",
                }
            )
            subprocess.run(
                ["/usr/bin/git", "init", "-q"],
                cwd=repository,
                env=environment,
                check=True,
            )
            (repository / "tracked.txt").write_text(
                "audit\n",
                encoding="utf-8",
            )
            subprocess.run(
                ["/usr/bin/git", "add", "--", "tracked.txt"],
                cwd=repository,
                env=environment,
                check=True,
            )
            subprocess.run(
                [
                    "/usr/bin/git",
                    "-c",
                    "core.hooksPath=/dev/null",
                    "commit",
                    "-q",
                    "-m",
                    "audit fixture",
                ],
                cwd=repository,
                env=environment,
                check=True,
            )

            success = subprocess.run(
                [
                    "/bin/bash",
                    "-c",
                    (
                        "set -euo pipefail\n"
                        + recipe
                        + '\ncleanup_audit\ntrap - EXIT HUP INT TERM\n'
                    ),
                ],
                cwd=repository,
                env=environment,
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(
                0,
                success.returncode,
                success.stderr,
            )
            self.assertEqual([], list(audit_parent.iterdir()))

            forced = subprocess.run(
                [
                    "/bin/bash",
                    "-c",
                    "set -euo pipefail\n" + recipe + "\nexit 23\n",
                ],
                cwd=repository,
                env=environment,
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(23, forced.returncode, forced.stderr)
            self.assertEqual([], list(audit_parent.iterdir()))


if __name__ == "__main__":
    unittest.main()
