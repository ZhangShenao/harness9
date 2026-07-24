from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
PR_SKILL = ROOT / ".agents" / "skills" / "pr" / "SKILL.md"


class PullRequestSkillTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.content = PR_SKILL.read_text(encoding="utf-8")

    def test_accepts_explicitly_authorized_existing_head(self) -> None:
        required_phrases = (
            "explicitly authorizes publishing the existing committed `HEAD`",
            "worktree and index are empty",
            "EXISTING_HEAD_AUTHORIZED",
            "COMMIT_HEAD=RECORDED_HEAD",
        )

        for phrase in required_phrases:
            with self.subTest(phrase=phrase):
                self.assertIn(phrase, self.content)

    def test_clean_worktree_alone_is_not_authorization(self) -> None:
        self.assertIn(
            "Never treat a clean worktree alone as authorization",
            self.content,
        )


if __name__ == "__main__":
    unittest.main()
