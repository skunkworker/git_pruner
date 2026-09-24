# git_pruner — analysis and improvement roadmap

A deep review of the codebase (2026-07-26), the fixes that came out of it, and the work that
remains. Each finding was reproduced in a throwaway repository before being recorded here;
findings that did **not** survive testing are listed at the bottom so they are not re-litigated.

## Design assessment

The safety model is the strongest part of this codebase and should be preserved as it evolves:

- a confirmation screen that itemizes every branch before anything is deleted
- a deliberate `y` (local) vs `R` (local + remote) split, so remote deletion is never one
  accidental keystroke
- `-d` → `-D` escalation via an explicit prompt rather than silent forcing
- merge status computed from local remote-tracking refs, so it needs no network
- deletions run off the update loop with a live per-branch checklist

The findings below are mostly about places where that model had a gap, not about its design.

---

## Completed

### Tier 1 (all done)

**1. Gone branches could silently discard unpushed commits.** *(the significant one)*

`%(upstream:track)` reports a bare `[gone]` with **no ahead count**, so `branch.ahead` parsed to
`0`. The chain: `p` auto-selected every gone branch → `confirmView`'s unmerged warning was gated
on `!br.gone && br.ahead > 0` and so never fired → `deleteFlag` returned `-D` unconditionally →
`y` destroyed the commits. The `-d`→`-D` force prompt never fired either, because `-D` succeeds
on the first try. Every other destructive path in the tool warns; this one — the headline `p`
workflow — did not.

Reproduced with a branch that was pushed, had its remote deleted, then accumulated local commits:

```
feature/important [origin/feature/important: gone]   track=[gone]  →  ahead parsed as 0
git rev-list --count main..feature/important         →  2 commits destroyed, no warning
```

Fixed by `riskCommitCount`, which measures each gone branch against the default branch. Gone
branches with no unique commits are still auto-selected by `p` (the one-keystroke workflow is
intact); ones holding unique commits are left unselected, reported in the status line, and
flagged on the confirmation screen.

*Why `git cherry` rather than `git rev-list <base>..<branch>`:* both were measured against a
squash-merged branch, a single-commit squash, and genuinely unmerged work:

| branch    | `rev-list --count` | `git cherry` `+` lines |
| --------- | ------------------ | ---------------------- |
| squashed (2 commits → 1) | 2 | 2 |
| single-commit squash     | 1 | **0** |
| genuinely unmerged       | 1 | 1 |

`git cherry` is strictly more accurate at the same cost — it recognizes cherry-picked, rebased,
and singly-squashed work as already integrated. It cannot detect a *group* squash, and nothing
cheap can. That residual over-report is why the warning is worded `N commit(s) not in <base>`
rather than claiming the work is unrecoverable.

**2. `truncate` sliced bytes, emitting invalid UTF-8.** `s[:w-1]` split multibyte runes:

```
truncate("日本語のコミットです", 9)  → "日本\xe8\xaa…"   validUTF8 = false
truncate("日本語のコミットです", 11) → "日本語\xe3…"     validUTF8 = false
```

Byte length also is not display width, so wide (CJK/emoji) columns were mis-sized in both
directions. Fixed with `ansi.Truncate` plus a new `pad` helper; `recomputeNameWidth` now measures
cells via `ansi.StringWidth`.

**3. The `gone` track value was 8 cells wide where every other value was 10**, shifting every
column after it on exactly the rows the user is there to act on. The regression test was verified
to fail against the old code (`date column at cell 31, want 33`) before being kept.

**4. Default-branch resolution hardcoded `origin`.** On a repo whose only remote was `upstream`,
`remoteDefault()` returned `""` and the `✓ merged` indicator plus the confirm-screen merge line
silently vanished — no error, the safety signal simply was not there. `remotes()` now tries every
configured remote with `origin` ordered first.

### Tier 2

**7. The tested delete path was not the one users run.** `performDeletions` was test-only by its
own comment, so the live async path's completion logic — `branchDeletedMsg` → `deletesDone` → the
`stateForcePrompt` / `stateResult` transition — was never fed through `Update`. The riskiest state
machine in the program was the untested one.

All 18 delete call sites now drive the real path through a `drainDeletions` helper, which runs the
`tea.Batch` cmds concurrently (as the runtime does) and feeds every `branchDeletedMsg` back through
`Update`. The force prompt is answered with a keystroke rather than by calling
`forceDeleteUnmerged` directly, and the four tests that hand-set `state = stateForcePrompt` now
assert the machine put them there. `performDeletions` has been deleted.

Five tests cover what only the async path can get wrong; each was verified to fail against a
deliberately broken `Update` before being kept:

| test | mutation it catches |
| ---- | ------------------- |
| `TestDeletingWaitsForEveryResult` | completing the run on the first result (`> 0` instead of `>= len(m.results)`) — and reloading the branch list mid-run, which would renumber the indices outstanding messages still write to |
| `TestStrayDeleteResultIsIgnored`  | dropping the `msg.idx` bounds check — panics with `index out of range [7]` |
| `TestSpinnerTickStopsAfterDeleting` | dropping the `state == stateDeleting` guard, leaving the tick re-arming forever |
| `TestForcePromptDeclineKeepsBranch` | `n` at the prompt discarding the branch it exists to spare |
| `TestDeletingIgnoresKeysExceptCtrlC` | a stray keystroke dismissing a run whose results have not landed |

**14. Startup and refresh spent over a second measuring risk, one branch at a time.** Every
`git cherry` is its own subprocess, and `refreshMergeInfo` ran one per gone branch in sequence —
on the tool's headline case, a repo full of gone branches, that is the whole list on the clock
before the first paint, again after every fetch, and again after every prune.

Measured on a 101-branch repo where 100 branches are gone:

| path | before | after |
| ---- | ------ | ----- |
| `initialModel` (startup)            | 1.14s | 0.56s |
| `refreshMergeInfo` (fetch / reload) | 1.12s | 0.33s |
| `measureSelectedRisk` (pressing `d`)| 1.09s | 0.25s |

The cost is process spawn, not git work: 100 × `git rev-parse HEAD` takes 0.63s against 100 ×
`git cherry` at 0.74s, so ~6ms per subprocess is the floor and no cheaper git command would have
helped. `measureRisk` now takes a predicate and runs the calls concurrently, one goroutine per
branch writing its own slice element. A cap sweep on the same repo put the knee at 4–8 workers
(1 → 1056ms, 4 → 364ms, 8 → 330ms, 32 → 331ms), so `maxLocalGit` is 8; past that the limit is
elsewhere.

**15. A wide prune opened one connection per branch.** `tea.Batch` gives every selected branch its
own goroutine (`bubbletea/tea.go:545`), so arming remotes on 100 branches meant 100 simultaneous
`git push --delete` — 100 connections a remote would throttle or refuse. A failed push there is
the worst shape the tool has: the local branch is already gone, and the copy that was meant to
outlive it is still sitting on the remote.

Local and network work are now capped separately — `maxLocalGit = 8`, `maxRemotePush = 3` — so a
wide local fan-out never widens the network one. The cap sits inside `runGit`, the single door
every git invocation already passes through, and `networkBound(args)` picks which one applies. Put
at the call sites instead it held only for the callers that remembered to ask: `fetchPruneCmd` and
`forceDeleteUnmerged` both escaped it.

The cap is measured on the subprocesses themselves, not on the limiter: `runGit` feeds two
`gauge`s, one for all git processes and one for the network-bound ones. Instrumenting the limiter
would have stopped reporting along with it — verified by deleting the limiter, which took the
observed peak from 3 to 17 and failed `TestWideDeleteStaysWithinCaps`. The test asserts literal
bounds rather than the constants, so raising a cap breaks it instead of moving with it.

The earlier "60 parallel deletes succeeded" note below still stands, but it only ever covered
local ref locking. It says nothing about the network.

**16. A `git cherry` subprocess per branch, for an answer one query already held.** `measureRisk`
ran `git cherry <base> <branch>` for every gone branch. A branch whose tip is already an ancestor
of the base has an empty `base..branch` range, so `git cherry` can only report nothing — and on
the tool's headline case, a repo full of branches that were merged and then pruned, that is nearly
every one of them.

`refreshMergeInfo` now runs one `git branch --merged <base>` and caches the result in
`m.baseMerged`; `measureRisk` reads the set instead of starting a process. Measured on the same
101-branch fixture (100 gone, all merged): startup **529ms → 45ms**, peak concurrent git
processes 8 → 2. One query at 9ms replaced 30 `git cherry` calls at 330ms on a 30-branch probe.

`TestMergedBranchesCostNoSubprocess` asserts a literal process bound rather than a time, and fails
at 39 processes when the shortcut is removed.

**17. The confirm screen rendered its whole body, pushing its own question off the terminal.**
`confirmView` wrote every selected branch in full. With 40 selected that is 206 rows into a 24-row
terminal, with `Delete these branches?` on row 205 — the user answered a prompt they could not
read. `forcePromptView`, `resultView` and `deletingView` had the same shape.

All four now go through `page(header, body, footer)`, which renders the header, a window into the
body, and the footer, plus a position line when the body does not fit. `scrollKeys` adds ↑/↓,
space, ctrl+d/u, pgup/pgdn and g/G, and the mouse wheel scrolls them; each screen answers its own
keys first, so `y`, `R` and `n` are never swallowed. Rendering only the visible rows also takes
the per-frame cost off the length of the list.

**18. Startup was six git subprocesses deep, and the depth was the whole cost.** On this repo
(3 branches) startup was 34ms, all of it process-start latency — one `git` costs ~6.5ms to spawn,
and the chain ran `rev-parse` → `for-each-ref` → `remote` → `symbolic-ref` → `branch -r --merged`
→ `branch --merged <base>` in sequence. Only `branch --merged HEAD` overlapped anything.

Three changes, together taking the chain to two rounds:

- `loadRemoteRefs` reads `refs/remotes` once with `--format='%(refname)%00%(symref)'`. That single
  call replaces `git remote`, a `symbolic-ref` per remote and a `rev-parse --verify` per candidate.
  `%(symref)` yields the full ref, so it keeps the tag-shadowing guarantee `symbolic-ref --short`
  would break. Remote names now come out of the refs: a remote with no fetched refs holds nothing
  a default branch could resolve to, so nothing is lost.
- `localDefaultBranch` takes a `has(name)` predicate. `refreshMergeInfo` passes a lookup into the
  branch list it already holds; only `baseBranch`, which has no list, still pays `gitHasBranch`.
- `loadRepo` starts `loadBranches`, `localMergedSet` and `loadRemoteRefs` in one round, and
  `initialModel` runs the repo check beside them rather than ahead of them. `refreshMergeInfo`
  then runs `branch -r --merged` and `branch --merged <base>` as a second round.

| repo | before | after |
| ---- | ------ | ----- |
| this one (3 branches)                | 34ms | 19ms |
| 101 branches, 100 gone (with item 16)| 529ms | 17ms |
| 501 branches                         | 90ms | 77ms |

The 501-branch case is `for-each-ref`'s own work (42ms of the 77ms), not chain depth — measured at
15ms for `%(refname)` alone against 42ms for the full format, so the commit-object reads behind
`%(committerdate)` are the floor there.

### Also completed

- `LICENSE` (MIT)
- `.github/workflows/ci.yml` — gofmt, `go build`, `go vet`, `go test -race` on Linux and macOS
- `.gitignore` — `go build ./...` drops a binary in the repo root

---

### 2026-09-23 pass (all of the remaining roadmap, plus new work)

**5. First paint no longer waits on the merge queries.** `startupModel` paints after round one;
`Init` runs `queryMergeInfo` (round two plus the gone-branch `git cherry` calls) as a `tea.Cmd`.
The result carries `loadGen`, so a reply for a list that was reloaded since is dropped. Until it
lands, `x`, `m` and `d` wait with a status line: selecting on unknown merge state is the one
thing that must not happen. Every other reload path stays synchronous (it is ~16ms), which keeps
one async path instead of a pending-action queue behind `p`. `initialModel` keeps the old
synchronous behavior for script mode and the tests. `v` now also loads its diff (and re-runs
delta on resize) in a `tea.Cmd`, with `diffSeq` dropping stale answers.

**6. Network calls are bounded.** `runGit` sets `GIT_TERMINAL_PROMPT=0`, starts `push`/`fetch`
in a new session (`Setsid`, so ssh has no `/dev/tty` to prompt on), and kills them after
`netTimeout` (60s) with `WaitDelay` for an ssh child still holding the pipes. Tested with a fake
`git` on `PATH` that sleeps, because the `ext::` transport needs a config override.

**8. Smaller items — all fixed.**
- Rows fit the terminal: `rowLayout` drops the relative date, then the hash, then the date until
  a 10-cell subject fits, then shrinks the name; a final `ansi.Truncate` guards the rest. The
  header, help, status and error lines are cut to the width too, and `visibleRows` counts the
  status and error lines (`TestListViewFitsHeight` checks the exact height).
- Subjects and git error text pass through `cleanText` (ANSI strip, control characters out);
  raw diff lines too, with tabs widened first. Branch names cannot hold control characters
  (`check-ref-format`), so they need nothing.
- The cursor starts on row 0: `installBranches` reads the focused name from the *old* list.
- A clean gone branch shows `✓ no commits missing from <base>` instead of `⚠ not merged`.
- ctrl+c while deleting needs a second press, with an on-screen warning.

**10. `m`: merged and older than N days.** Prompt starts at `pruner.staleDays` (default 90).
Merged means tip in the base, or upstream merged with `ahead == 0` — a branch ahead of its merged
upstream holds work the upstream check cannot see.

**11. Undo and recovery hints.** `u` (results screen or list) recreates the last run's local
branches at their full SHA and restores `branch.<name>.remote/.merge` from
`%(upstream:remotename)`/`%(upstream:remoteref)`, read in the existing `for-each-ref`. Remote
deletions are not undone (that would be a push); instead the exit summary prints
`git push <remote> <sha>:refs/heads/<b>`. The results screen no longer quits on `enter`. Undo runs as a `tea.Cmd`, serially: parallel
`git config` writes would collide on `.git/config.lock`.

A later `/simplify` pass also debounced delta on resize (100ms), so a window drag runs it once.
It left two findings on purpose: dropping the test-only `riskCommitCount`/`loadDiff` wrappers
(churn in a dozen tests for no behavior change), and keying the merge sets by full ref (it only
matters for a local branch literally named like a remote one, e.g. `origin/x`).

**12. `main.go` split** into `git.go`, `model.go`, `view.go`, `cli.go`, `settings.go`,
`proc_*.go` by an AST script — no hand edits in the move. **13.** `BINDIR` was already
overridable; the Makefile now rebuilds on any `*.go` change.

**New: locked branches.** `branch.locked()` = current, checked out in another worktree
(`%(worktreepath)`, shown `+` like `git branch`), or protected (the trunk plus `pruner.protect`
globs, shown `P`). Locked rows cannot be marked by any key, and `selectedBranches` drops them as a
second guard.

**New: remote-only view (`tab`).** Remote branches no local branch tracks, as rows in the same
slice with `remoteOnly` set; `viewIdx` filters by mode, so marks survive a view switch like they
survive a filter. The rows are read lazily (`loadRemoteRefs(true)`) on first `tab`, then kept on
every reload. Deleting them is `R` only; `c` runs `git switch -c <b> --track refs/remotes/…`
(the full ref, so a same-named tag cannot shadow it).

**New: script mode** (`--prune-gone`, `--merged-older-than`, `--fetch`, `--yes`, `--dry-run`).
Lists unless `--yes`; never deletes remotes.

**New: sort memory** in `os.UserConfigDir()/git_pruner/settings`; `settingsPath` stays empty
under test.

### Reviewed and deferred (2026-08-31 /simplify pass)

Two efficiency findings from the four-agent review of the `c` checkout and `/` filter change.
Both were skipped deliberately: each trades an unmeasurable speed gain for a new way to show
stale data. Recorded here so the analysis is not redone from scratch.

**D1. Light reload after a branch switch.** A `c` press costs 5 fixed git calls plus one
`git cherry` per gone branch (~100 calls, ~100 ms on the 100-gone-branch repo) — all in the
background since the switch runs as a `tea.Cmd`. A checkout moves HEAD only, so
`remoteMerged`, `riskCommits`, the risk base and `m.baseMerged` are provably unchanged; a
light path would run only `loadBranches()` + `localMergedSet()` (2 calls) and carry the rest
across by name, refreshing just `isCurrent` and `headMerged`. **Why deferred:** the carried
field list is an unchecked claim ("all fields a checkout cannot change"); anyone extending
`refreshMergeInfo` must remember to update it, or `c` becomes the one path that shows stale
data. The full reload is right by construction. **If ever done:** pin the carried fields with
a test that compares the light path against a fresh full reload on the same repo. This is the
only deferred item worth revisiting, and only if a real repo shows the background reload
contending with something.

**D2. `viewIdx` allocation and lowercase caching.** `viewIdx` rebuilds its `[]int` and
lowercases every name on each call, two to four times per keystroke; at 200 branches that is
~1.6 KB and 20–40 µs per build — about 100× below the program's smallest felt cost (a 6 ms
subprocess spawn). The proposed fix (`visibleCount()`/`visibleAt(p)` helpers, cached
`lowerName` per branch, cached lowered filter) needs three predicate copies kept identical
plus two cache-refill rules; missing a refill on a future load path makes the filter silently
drop rows. **Why deferred:** a stale cache is a bug class, a microsecond is not. A risk-free
middle option — sharing one `viewIdx()` result within a call chain — was judged not worth the
churn either.

---

## Investigated and rejected

**A commit subject spanning several lines breaking the `for-each-ref` parse.** `loadBranches`
splits output on newlines and needs 8 NUL-separated fields per branch, so an embedded newline
would silently drop branches from the list. It cannot happen: `%(contents:subject)` folds the
subject's newlines to spaces. Verified against a commit whose subject wraps before the blank line.

**A duplicate or stray `branchDeletedMsg` ending a run early.** `deletesDone` counts `done` flags
on the results, not messages received, so a repeat cannot over-count; an out-of-range index is
dropped by the bounds check. Both are covered by `TestStrayDeleteResultIsIgnored`.

**Concurrent `git branch -d` racing on `packed-refs.lock`.** `tea.Batch` runs deletions
concurrently, which looked like it should collide on the packed-refs lock. Tested with 60 parallel
deletes against a freshly packed repo: **all 60 succeeded.** Git's ref-lock retry handles it. No
change needed — recorded so it is not re-investigated.
