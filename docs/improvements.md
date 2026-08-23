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

### Also completed

- `LICENSE` (MIT)
- `.github/workflows/ci.yml` — gofmt, `go build`, `go vet`, `go test -race` on Linux and macOS
- `.gitignore` — `go build ./...` drops a binary in the repo root

---

## Remaining work

### Tier 2 — robustness

**5. Blocking git calls inside `Update`.** `loadDiff` (`v`), `refreshMergeInfo` and
`reloadBranches` still run synchronously in the update loop, so what remains of their cost is
still a freeze. The per-branch measurement that dominated them has been parallelised (see item 14
under Completed); the residue is ~370ms on a 100-gone-branch repo. Moving them onto the `tea.Cmd`
pattern already used for fetch would take that to zero.

Do not reach for `git branch -r --merged` first: it was measured at 100ms with 2000 remote
refs, roughly a tenth of what the `git cherry` loop beside it cost.

**6. `runGit` has no timeout and does not disable terminal prompts.** `fetch --all --prune` and
`push --delete` are network-bound; a credential or SSH prompt hangs the TUI with no recovery.
Set `GIT_TERMINAL_PROMPT=0` and attach a `context.WithTimeout` so it fails fast instead.

**8. Smaller items.**
- Rows are wider than the terminal whenever `subjectWidth` hits its `max(10, …)` floor, and each
  wrapped row eats two screen lines while `visibleRows` still counts it as one — so the list
  overruns and the footer scrolls away. Measured threshold: rows wrap below `68 + nameW` columns,
  which at a classic 80-column terminal means any branch name of 13 cells or more wraps *every*
  row. `renderRow` needs to fit `m.width` rather than assume it.
- `listView` runs one line over terminal height when `status` and `err` are both set
  (`visibleRows` is `height-5`; actual emission is `height+1`) — confirmed at 25 lines for a
  height of 24.
- ANSI and control characters in commit subjects and branch names render raw into the terminal.
  Confirmed: a `\x1b[31m` in a subject reaches the row intact (a bare `BEL` is stripped by
  `ansi.Truncate`). The text comes from fetched branches, so it is not the author's to trust.
- `applyBranches` silently discards the user's existing selections on `p`.
- The cursor starts on an arbitrary row. `sortBranches` preserves the cursor by name
  unconditionally, but at startup `cursor` is 0 and `branches` is still in `for-each-ref`
  (alphabetical) order, so it pins the cursor to wherever the alphabetically-first branch
  lands after sorting — row 10 of 11 on the demo repo. Skip the preserve when there is no
  prior cursor to restore.
- The confirmation screen warns `⚠ not merged into <default>` for every gone branch, because
  `remoteMerged` tests the upstream ref and a gone branch no longer has one. Branches that were
  merged and pushed before their upstream was deleted are flagged as if they held unique work;
  `riskWarning` already reports the real cost correctly.
- `stateDeleting`'s ctrl+c quits while `git push --delete` children are still running.

### Tier 3 — features for the tool's actual job

**9. `/` incremental filter.** With dozens of branches there is currently no way to narrow the
list — the single biggest UX gap for the repos this tool exists to clean up.

**10. Bulk-select predicates** (merged, older than N days). "Select everything merged and older
than 90 days" is the canonical prune workflow and currently has to be done by hand.

**11. Reflog recovery hint after a `-D`.** The force-prompt screen says "permanently discard their
unmerged commits" without telling the user that `git reflog` can still recover them. Pairs
naturally with finding 1.

### Tier 4 — hygiene

**12. Split `main.go`** (~1,300 lines) into `git.go` / `model.go` / `view.go`.

**13. Make the Makefile's `BINDIR` overridable** — it hardcodes `$HOME/shared/bin`.

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
