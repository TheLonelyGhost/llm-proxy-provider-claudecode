---
name: cut-release
description: >
  Cut a release: verify clean main branch, derive a new semver from CHANGELOG.md Unreleased section, propose and confirm changelog + manifest edits, commit, tag, and prompt for push. Use when the user says "cut a release", "release", "tag a version", or "bump version".
---

# Cut Release

Walk through every step in order. **Do not skip or reorder steps.** If any step fails, stop and report what went wrong before going further.

---

## Step 1 — Verify branch and working copy

Run the helper script from the repo root:

```sh
bash .agents/skills/cut-release/scripts/verify-up-to-date.sh
```

If it exits non-zero, abort and surface its stderr to the user. Only proceed once it exits 0.

---

## Step 2 — Read current version and Unreleased section

**Manifest:** find the manifest file:

```sh
ls manifest/*.manifest.json
```

There should be exactly one file. Read it and extract the `"version"` field. Call this `CURRENT_VERSION`.

**Changelog:** read `CHANGELOG.md`. Extract everything under the `## [Unreleased]` heading down to (but not including) the next `## ` heading. Call this the _Unreleased block_.

**Recommend a new version** using these rules (applied in priority order):

| Condition | Bump |
|-----------|------|
| Unreleased block contains a `### Removed` or any `**BREAKING:**` marker | MAJOR |
| Unreleased block contains a `### Added` section with at least one bullet | MINOR |
| Unreleased block contains only `### Changed`, `### Deprecated`, `### Fixed`, or `### Security` sections | PATCH |
| Unreleased block is empty or has no bullets under any heading | Abort — _"Nothing in Unreleased to release."_ |

Apply the bump to `CURRENT_VERSION` following [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Call the result `NEW_VERSION`. Do **not** add a `v` prefix to the version string stored in the manifest.

**Report** (no edits yet):

```
Current version : <CURRENT_VERSION>
Unreleased scope: <MAJOR|MINOR|PATCH>
Proposed version: <NEW_VERSION>
```

---

## Step 3 — Propose changes and confirm

Produce the two diffs below and present them to the user.

### CHANGELOG.md diff

The only edits are:

1. Replace `## [Unreleased]` with `## [Unreleased]\n\n## [<NEW_VERSION>] — <TODAY_ISO>` where `<TODAY_ISO>` is `date -u +%Y-%m-%d`.
2. Replace the bare comparison link at the bottom:
   ```
   [Unreleased]: https://github.com/…
   ```
   with two lines:
   ```
   [Unreleased]: https://github.com/…/compare/v<NEW_VERSION>...HEAD
   [<NEW_VERSION>]: https://github.com/…/compare/v<CURRENT_VERSION>...v<NEW_VERSION>
   ```
   Derive the repo URL from the existing link — do not hard-code it.

### Manifest diff

Change `"version": "<CURRENT_VERSION>"` to `"version": "<NEW_VERSION>"` in the manifest file. No other fields change.

Present both diffs as fenced `diff` blocks. Then ask:

> Apply these changes and create the release commit and tag? [y/N]

---

## Step 4 — Gate on human consent

Read the user's reply literally.

- If the reply is exactly `y` or `Y` (case-insensitive), continue.
- Any other reply — including silence, `yes`, `n`, `N`, `no`, blank — counts as **no**. Abort: _"Release aborted. No changes were made."_

---

## Step 5 — Apply changes

Apply the edits described in Step 3 to `CHANGELOG.md` and the manifest file. Do not touch any other file.

---

## Step 6 — Stage only the two changed files

```sh
git add CHANGELOG.md manifest/<name>.manifest.json
```

Confirm with `git diff --cached --stat` that exactly those two files are staged and nothing else. If other files are staged, abort and undo with `git restore --staged .`.

---

## Step 7 — Commit

```sh
git commit --no-edit -m "Bumps to version <NEW_VERSION>"
```

The `--no-edit` flag prevents an editor from opening. The commit message is exactly `Bumps to version <NEW_VERSION>` — no body, no trailer, no co-authorship line.

---

## Step 8 — Tag

```sh
git tag "v<NEW_VERSION>"
```

Verify the tag exists with `git tag --list "v<NEW_VERSION>"`.

---

## Step 9 — Prompt for push

Print the following message verbatim (substituting `<NEW_VERSION>`). Do **not** run these commands:

```
Release v<NEW_VERSION> is committed and tagged locally.

To publish, run:

    git push origin main --tags
```
