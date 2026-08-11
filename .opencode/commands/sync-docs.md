---
description: Sync bilingual docs between docs/核心功能/ (Chinese) and docs/core-features-en/ (English).
---

# Sync Bilingual Docs

## Overview

Ensure every Chinese doc in `docs/核心功能/` has a corresponding English doc in `docs/core-features-en/` with matching filename and aligned content. This is part of the project's Definition of Done--a feature is not complete without bilingual docs.

## When to Run

- After creating or modifying any file in `docs/核心功能/`
- Before committing a feature that includes documentation changes
- When the user says `/sync-docs`

## Workflow

### 1. Scan for mismatches

Run this check to identify missing or stale English docs:

```bash
for f in docs/核心功能/*.md; do
  name=$(basename "$f")
  en_path="docs/core-features-en/$name"
  if [ ! -f "$en_path" ]; then
    echo "MISSING: $en_path"
  elif [ "$f" -nt "$en_path" ]; then
    echo "STALE: $en_path (Chinese version is newer)"
  fi
done
```

Classify results:
- **MISSING**: English doc does not exist--must be created
- **STALE**: English doc exists but Chinese version was modified more recently--should be reviewed and updated
- **OK**: Both exist and English is not older than Chinese

If all docs are OK, report success and exit.

### 2. For each MISSING doc: create the English version

Read the Chinese doc in full. Then create the English counterpart at `docs/core-features-en/<same-filename>` following these rules:

**Translation principles:**
- The English version is an **independently written technical document**, not a machine translation
- Content and structure must align with the Chinese version (same sections, same code blocks, same tables)
- Follow English technical writing conventions: active voice, concise sentences, no unnecessary hedging
- Code blocks, Go struct definitions, and CLI examples remain identical (they are language-neutral)
- Chinese-specific UI strings shown in examples (e.g., TUI output) should be translated to English equivalents
- Comments in code blocks: translate Chinese comments to English

**File header:** Start with an English `# Title` that translates the Chinese title.

### 3. For each STALE doc: review and update

Read both the Chinese and English versions. Compare section by section:
- If the Chinese version added new sections, add corresponding English sections
- If the Chinese version modified content, update the English to match
- If the Chinese version removed content, remove the corresponding English content
- Preserve any English-specific phrasing that is already good

### 4. Verify

After creating/updating, re-run the scan from step 1. Require zero MISSING and zero STALE results.

Also verify that `website/scripts/sync-docs.mjs` will pick up the new files correctly (it scans both directories for `*.md` files with matching names).

### 5. Report

Report:
- Files created (MISSING -> created)
- Files updated (STALE -> updated)
- Files already in sync (OK)
- Total doc count in each directory (should match)

## Constraints

- Never delete an English doc without explicit user instruction
- Never modify the Chinese docs--this command only creates/updates English counterparts
- If a Chinese doc has no corresponding English doc and the content is too large to translate in one pass, ask the user whether to proceed or split the work
