#!/usr/bin/env bash
# dry_run_sync.sh
#
# Simulates what sync_version_branches does on merge to main.
# For each release branch:
#   1. Performs a throwaway local merge of the MR source branch (never pushed)
#   2. Sets up the same Go environment the real test_and_lint job uses
#   3. Runs `make ci` against the merged state — the exact same entrypoint
#      as test_and_lint — so failures here mean failures after the real sync
#
# Required environment variables (all provided automatically by GitLab CI):
#   CI_MERGE_REQUEST_SOURCE_BRANCH_NAME  — the developer's MR branch
#   CI_MERGE_REQUEST_IID                 — MR number, used in merge commit message
#   CI_JOB_ID                            — used to make throwaway branch names unique
#   CI_PROJECT_DIR                       — repo root, used for Go cache paths
#   CI_SERVER_HOST                       — used for GOPRIVATE and .netrc
#   CI_JOB_TOKEN                         — used for .netrc private module auth
#   SUBPROJECT_REL_PATH                  — set by template, defaults to "./"
#   RELEASE_BRANCHES                     — space-separated list, e.g. "release-0.31 release-0.32 release-0.33"

set -euo pipefail

# ── Validate required env vars ────────────────────────────────────────────────
: "${CI_MERGE_REQUEST_SOURCE_BRANCH_NAME:?Must be run in a GitLab MR pipeline}"
: "${CI_MERGE_REQUEST_IID:?Must be run in a GitLab MR pipeline}"
: "${CI_JOB_ID:?Must be run in a GitLab CI pipeline}"
: "${CI_PROJECT_DIR:?Must be run in a GitLab CI pipeline}"
: "${CI_SERVER_HOST:?Must be run in a GitLab CI pipeline}"
: "${CI_JOB_TOKEN:?Must be run in a GitLab CI pipeline}"
: "${RELEASE_BRANCHES:?RELEASE_BRANCHES must be set, e.g. 'release-0.31 release-0.32 release-0.33'}"

SUBPROJECT_REL_PATH="${SUBPROJECT_REL_PATH:-./"}"

# ── Git setup (read-only — no push token used here) ──────────────────────────
git config user.email "ci@crusoe.ai"
git config user.name "GitLab CI"

# Configure private module access — mirrors .private-repo-script from template
go env -w "GOPRIVATE=${CI_SERVER_HOST}"
echo -e "machine ${CI_SERVER_HOST} login gitlab-ci-token password ${CI_JOB_TOKEN}" > "${HOME}/.netrc"

# Fetch all release branches so we can check them out locally
git fetch origin '+refs/heads/release-*:refs/remotes/origin/release-*'

# ── Go cache setup — mirrors .cache-setup from template ──────────────────────
mkdir -p .cache
export GOMODCACHE="${CI_PROJECT_DIR}/.cache/go-modules"
export GOPATH="${CI_PROJECT_DIR}/.cache/go"
mkdir -p "${GOMODCACHE}" "${GOPATH}"
export PATH="${PATH}:${GOPATH}/bin"

# ── Per-branch dry-run loop ───────────────────────────────────────────────────
failed=""

for branch in ${RELEASE_BRANCHES}; do
  echo ""
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  echo "  Dry-run: ${CI_MERGE_REQUEST_SOURCE_BRANCH_NAME} → ${branch}"
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

  # Unique name per branch + job so parallel jobs never collide
  work_branch="ci-dry-run-${branch}-${CI_JOB_ID}"

  # ── Step 1: Create throwaway local branch from the release branch ────────
  if ! git checkout -b "${work_branch}" "origin/${branch}" 2>&1; then
    echo "❌ Could not checkout origin/${branch}"
    echo "   Does this branch exist? Check RELEASE_BRANCHES is up to date."
    failed="${failed} ${branch}(checkout)"
    continue
  fi

  # ── Step 2: Simulate the exact merge sync_version_branches will perform ──
  # Uses --no-ff to match the real sync job exactly
  if ! git merge --no-ff "origin/${CI_MERGE_REQUEST_SOURCE_BRANCH_NAME}" \
        -m "ci: dry-run merge for MR !${CI_MERGE_REQUEST_IID}" 2>&1; then

    git merge --abort 2>/dev/null || true
    echo ""
    echo "❌ CONFLICT: Your branch cannot cleanly merge into ${branch}."
    echo "   This would break sync_version_branches when this MR lands on main."
    echo ""
    echo "   To reproduce and fix locally:"
    echo "     git fetch origin"
    echo "     git checkout -b debug-${branch} origin/${branch}"
    echo "     git merge origin/${CI_MERGE_REQUEST_SOURCE_BRANCH_NAME}"
    echo "     # resolve conflicts, commit, then push your branch again"
    failed="${failed} ${branch}(conflict)"

    # Clean up and move on to the next branch
    git checkout "${CI_MERGE_REQUEST_SOURCE_BRANCH_NAME}" --quiet 2>/dev/null || true
    git branch -D "${work_branch}" 2>/dev/null || true
    continue
  fi

  echo "✅ Merge simulation succeeded for ${branch}"
  echo ""

  # ── Step 3: Report which go.mod we're testing against ───────────────────
  # This is the critical check — the release branch go.mod may have a
  # different Go version and dependency versions than main. `make ci` will
  # compile and lint against whatever go.mod is present in the working tree.
  BRANCH_GO_VERSION=$(awk '/^go /{print $2}' go.mod 2>/dev/null || echo "unknown")
  echo "→ go.mod Go version for ${branch}: ${BRANCH_GO_VERSION}"
  echo "→ Running: make ci (same entrypoint as test_and_lint)"
  echo ""

  # ── Step 4: Run make ci against the merged state ─────────────────────────
  # This is identical to what test_and_lint runs:
  #   cd ${SUBPROJECT_REL_PATH} && make ci
  # If make ci produces golangci-lint.json on failure, we log it for
  # visibility (mirroring the jq severity-patching in the template).
  lint_json="${SUBPROJECT_REL_PATH}golangci-lint.json"

  if ! (cd "${SUBPROJECT_REL_PATH}" && make ci) 2>&1; then
    echo ""
    if [ -f "${lint_json}" ]; then
      echo "↳ golangci-lint findings (first 40 lines):"
      head -40 "${lint_json}" || true
    fi
    echo ""
    echo "❌ make ci FAILED on merged state of ${branch}."
    echo "   Your changes pass on main (Go: $(awk '/^go /{print $2}' go.mod 2>/dev/null || echo 'unknown'))"
    echo "   but fail after merging with ${branch} (Go: ${BRANCH_GO_VERSION})."
    echo "   This would break the build after sync_version_branches runs."
    failed="${failed} ${branch}(make-ci)"
  else
    echo "✅ make ci passed for ${branch}"
  fi

  # ── Cleanup: delete throwaway branch, return to source branch ───────────
  # We never push — this entire operation is local and read-only on origin
  git checkout "${CI_MERGE_REQUEST_SOURCE_BRANCH_NAME}" --quiet 2>/dev/null || true
  git branch -D "${work_branch}" 2>/dev/null || true
done

# ── Final summary ─────────────────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [ -n "${failed}" ]; then
  echo "❌ dry_run_sync FAILED — branches with issues: ${failed}"
  echo ""
  echo "Fix the above before merging. These failures will occur in"
  echo "sync_version_branches the moment this MR lands on main."
  echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
  exit 1
fi

echo "✅ All release branches will sync and build cleanly from main."
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"