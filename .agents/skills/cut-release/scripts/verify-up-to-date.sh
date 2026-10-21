#!/usr/bin/env bash
# verify-up-to-date.sh
# Step 1 of the cut-release skill: verify the working copy is on main,
# up-to-date with origin, and has no uncommitted or untracked changes.
# Exits 0 on success; exits 1 and prints an actionable message on failure.
set -euo pipefail

# --- 1. Must be on main ---------------------------------------------------
branch=$(git rev-parse --abbrev-ref HEAD)
if [ "$branch" != "main" ]; then
  echo "ERROR: Not on main branch (currently on \`${branch}\`). Checkout main and retry." >&2
  exit 1
fi

# --- 2. Pull must succeed -------------------------------------------------
if ! pull_output=$(git pull origin main 2>&1); then
  echo "ERROR: \`git pull origin main\` failed:" >&2
  echo "$pull_output" >&2
  exit 1
fi

# --- 3. Working copy must be clean (no staged, modified, or untracked) ----
dirty=$(git status --porcelain)
if [ -n "$dirty" ]; then
  echo "ERROR: Working copy has uncommitted changes. Stash or commit them and retry." >&2
  exit 1
fi
