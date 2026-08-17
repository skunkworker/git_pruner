#!/usr/bin/env bash
# Builds a throwaway repo + bare "origin" so git_pruner has realistic branch
# state to render: merged branches, gone upstreams, unmerged work, no-upstream.
#
# macOS only: the date arithmetic below uses BSD `date -v`.
set -euo pipefail

ROOT="${1:?usage: make-demo-repo.sh <dir>}"
rm -rf "$ROOT"
mkdir -p "$ROOT"
REMOTE="$ROOT/origin.git"
WORK="$ROOT/orbital"

git init --quiet --bare -b main "$REMOTE"
git init --quiet -b main "$WORK"
cd "$WORK"

git config user.name "Ada Reyes"
git config user.email "ada@example.com"
git config commit.gpgsign false
git remote add origin "$REMOTE"

# Every commit gets an explicit date so the relative-time column reads naturally.
commit() { # commit <days-ago> <subject> <file>
  local days="$1" subject="$2" file="$3"
  local when
  when=$(date -u -v-"${days}"d +"%Y-%m-%dT%H:%M:%S")
  mkdir -p "$(dirname "$file")"
  printf '// %s\npackage orbital\n' "$subject" >> "$file"
  git add -A
  GIT_AUTHOR_DATE="$when" GIT_COMMITTER_DATE="$when" git commit --quiet -m "$subject"
}

branch_from_main() { git checkout --quiet -B "$1" main; }

merge() { # merge <days-ago> <branch>
  local when
  when=$(date -u -v-"$1"d +"%Y-%m-%dT%H:%M:%S")
  GIT_AUTHOR_DATE="$when" GIT_COMMITTER_DATE="$when" \
    git merge --quiet --no-ff -m "Merge branch '$2'" "$2"
}

# --- main line -------------------------------------------------------------
commit 210 "Initial commit: orbital service skeleton" README.md
commit 190 "Add HTTP router and health endpoint" src/router.go
commit 150 "Wire Postgres connection pool" src/store/pool.go
commit 96  "Add structured logging middleware" src/middleware/log.go
commit 61  "Support cursor pagination on /events" src/api/events.go
git push --quiet -u origin main

# --- merged into main, remote still present (safe -d) ----------------------
branch_from_main feature/rate-limiter
commit 44 "Add token-bucket rate limiter" src/middleware/ratelimit.go
commit 43 "Rate limiter: per-tenant buckets" src/middleware/ratelimit.go
git push --quiet -u origin feature/rate-limiter

branch_from_main chore/bump-deps
commit 38 "Bump golang.org/x/net to 0.38.0" go.mod
git push --quiet -u origin chore/bump-deps

branch_from_main fix/timezone-parsing
commit 30 "Parse RFC3339 offsets without truncating" src/api/time.go
git push --quiet -u origin fix/timezone-parsing

git checkout --quiet main
merge 23 feature/rate-limiter
merge 22 chore/bump-deps
merge 21 fix/timezone-parsing
commit 20 "Cache tenant lookups for 30s" src/store/tenant.go
git push --quiet origin main

# --- upstream deleted on the remote (shows as "gone" after p) --------------
branch_from_main feature/webhook-retries
commit 27 "Retry webhooks with exponential backoff" src/webhook/retry.go
commit 26 "Cap webhook retries at 5 attempts" src/webhook/retry.go
git push --quiet -u origin feature/webhook-retries

branch_from_main fix/session-leak
commit 24 "Close idle sessions on shutdown" src/store/session.go
git push --quiet -u origin fix/session-leak

branch_from_main release/v2.4.0
commit 18 "Release v2.4.0" CHANGELOG.md
git push --quiet -u origin release/v2.4.0

# Merge them so they carry no unique work, then delete the remote refs: this is
# exactly the state `p` (fetch --prune) is meant to surface.
git checkout --quiet main
merge 17 feature/webhook-retries
merge 16 fix/session-leak
merge 15 release/v2.4.0
git push --quiet origin main

# Delete the upstreams inside the bare repo rather than with `push --delete`,
# which would also drop the local remote-tracking refs and make the branches read
# as gone before the demo ever runs. This way they stay "stale but not yet
# pruned" — the state pressing `p` is there to resolve.
git -C "$REMOTE" branch -q -D feature/webhook-retries fix/session-leak release/v2.4.0

# --- unmerged work, upstream alive (ahead > 0, needs -D) -------------------
branch_from_main feature/oauth-device-flow
commit 9 "Add device authorization grant" src/auth/device.go
commit 7 "Poll token endpoint with backoff" src/auth/device.go
git push --quiet -u origin feature/oauth-device-flow
commit 3 "WIP: verification_uri_complete" src/auth/device.go

branch_from_main feature/audit-log
commit 12 "Append-only audit log writer" src/audit/writer.go
git push --quiet -u origin feature/audit-log
commit 5 "Redact PII from audit entries" src/audit/redact.go

# --- no upstream at all ----------------------------------------------------
branch_from_main spike/graphql-gateway
commit 34 "Spike: graphql gateway in front of REST" src/gateway/schema.go

branch_from_main refactor/storage-adapter
commit 2 "Extract storage behind an adapter interface" src/store/adapter.go

git checkout --quiet main
commit 1 "Emit request IDs on every response header" src/middleware/reqid.go
git push --quiet origin main
# Deliberately no `fetch --prune` here: the deleted upstreams must stay
# unpruned so pressing `p` in the demo is what reveals them as gone.
echo "demo repo ready: $WORK"
