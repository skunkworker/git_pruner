# git_pruner — test coverage gaps

An analysis of `main_test.go` (2026-08-17) proposing tests for everyday git edge cases the
suite does not cover. Git-behavior claims below were reproduced in throwaway repositories
before being recorded, following the same rule as `improvements.md`.

## Current coverage assessment

The 25 existing tests are strong on the deletion-safety core: safe-vs-force semantics
ground-truthed against real git (`TestSafeDeletableMatchesGit`), gone-branch risk guarding,
deferred remote deletes, the force-retry flow, and rendering width invariants.

What is missing is almost entirely **environmental variation**: every test runs in the same
happy-path repo shape — clean checkout on `main`, one healthy file-URL remote named
`origin`/`upstream`, born HEAD, no tags.

---

## Tier 1 — expose confirmed or likely bugs — **DONE 2026-08-17**

### 1. Tag shadowing a branch name *(was a confirmed bug — fixed)*

A tag sharing a branch's name silently shadowed the branch: git's ref search order puts
`refs/tags/<name>` ahead of `refs/heads/<name>`, and `riskCommitCount` / `loadDiff` passed
bare names to `git cherry` and `git diff`. Reproduced: `git cherry main feat` reported
**zero** at-risk commits for a branch that had one, with only a `refname 'feat' is
ambiguous` warning on stderr — so the risk warning said nothing was at stake on exactly the
branch a `-D` was about to destroy. Tagging a release branch with its own name (`v1.2`,
`release-3`) makes this an everyday shape.

Writing the test exposed a **deeper root cause than the bare-name calls**: `loadBranches`
read `%(refname:short)`, which returns the shortest *unambiguous* name. The moment the tag
exists, the branch loads as `heads/feature/x` rather than `feature/x`, which breaks every
name-keyed lookup and any ref rebuilt from the name. `%(upstream:short)` has the same
failure, disambiguating to `remotes/origin/x` and splitting into a bogus remote named
`remotes`.

Fixed by reading full refs and stripping the namespace ourselves:

- `shortRef()` strips `refs/heads/` / `refs/remotes/`; `loadBranches` now reads
  `%(refname)` and `%(upstream)`, and `mergedSet` reads `%(refname)`.
- `branchRef()` qualifies a local branch name for git; used by `riskCommitCount` and
  `loadDiff`.
- `qualifyRef()` probes `refs/heads/` then `refs/remotes/` for refs whose namespace is not
  known statically (the base). Resolved once into the new `model.riskBaseRef`, since it
  costs up to two `rev-parse` calls.

`riskBase` stays short for display; git is handed the qualified form. `git branch -d` was
never affected — it always operates in `refs/heads/`.

Covered by `TestTagShadowingBranchName` (`branch_name`, `upstream_name`, `base_name`).

### 2. Selections survive `p` *(was a real wipe — fixed)*

`fetchDoneMsg` called `applyBranches` with freshly loaded structs, so every manually
selected / `R`-armed branch was silently wiped when the user pressed `p`. A fetch mutates
nothing local, so the marks now survive it: `carryMarks()` copies `selected` and
`deleteRemote` onto the new set by name.

One deliberate exception: an armed remote delete is **dropped** when the fetch reveals the
upstream is gone, because the push it would run can only fail.

Covered by `TestFetchPreservesSelections` and `TestFetchDisarmsRemoteForGoneBranch`.

### 3. Gone *current* branch *(behavior was already correct — now locked in)*

The user is sitting on `feature/x`, the PR merged, the remote branch was deleted, and they
press `p`. The fetch handler's `isCurrent` guard already kept it out of auto-selection and
out of the "press d to prune" count, but nothing tested it. The test also pins the
second line of defence: selected by hand, git refuses the delete, and because the refusal
is not "not fully merged" it correctly does **not** raise the force prompt.

Covered by `TestGoneCurrentBranchIsNotPruned`.

---

## Tier 2 — everyday repo environments never constructed in tests — **DONE 2026-08-17**

No new production bugs here: every shape already behaved correctly, and the tests now pin
that. Two of the predictions above were wrong about git's behavior and were corrected
against real repos before being written up.

### 4. No remotes at all *(covered by `TestLocalOnlyRepo`)*

A scratch or never-pushed project. `initialModel` works, `riskBase` falls back to local
`main` (with `riskBaseRef` `refs/heads/main`), `loadDiff` bases on local `main`, the risk
warning still names the real cost, and `p` succeeds as a no-op — `git fetch --all --prune`
exits 0 with no remotes configured.

### 5. Unborn HEAD *(covered by `TestUnbornHeadRepo`)*

`git init`, zero commits: empty branch list, no base resolves, `listView` renders the empty
message, and every cursor/selection key is a no-op rather than a panic. `git branch --merged
HEAD` hard-fails here (`fatal: malformed object name HEAD`); `mergedSet` absorbs it.

### 6. Detached HEAD *(covered by `TestDetachedHead`)*

Mid-bisect, mid-rebase, or on a checked-out tag. No branch is `isCurrent`, so `a` selects
everything — there is no current branch to spare — and `headMerged` is computed against the
detached commit. Deleting the branch HEAD is parked on is legal and safe: the commits stay
reachable from HEAD.

Noted: `git branch --merged HEAD --format=%(refname)` emits a `(HEAD detached at …)`
pseudo-entry, which lands in `mergedSet` as a junk key. Harmless — branch names cannot
contain spaces, so it can never collide with a real lookup.

### 7. Remote default is neither `main` nor `master`, `origin/HEAD` unset *(covered by `TestNonStandardDefaultBranch`)*

E.g. `trunk`/`develop`. `origin/HEAD` only exists after a clone, not after `remote add` +
push — verified: `symbolic-ref refs/remotes/origin/HEAD` fails with `not a symbolic ref`.
With no `origin/main`, `origin/master`, or local `main`/`master` either, `riskBase == ""`,
which is the path both "no base branch to compare against" messages hang off. Now asserted
in `riskWarning`, `confirmView`, and `forcePromptView`.

The second subtest covers `remoteDefault`'s `symbolic-ref` path, which had **no coverage at
all** — `setupRepo` never sets `origin/HEAD`. After `git remote set-head origin trunk` it
resolves to `origin/trunk`.

### 8. Upstream's remote no longer exists *(covered by `TestUnreachableRemote`)*

**Correction:** the prediction that `git remote remove origin` leaves the branch showing
"gone" is wrong. Removing a remote also unsets `branch.<name>.remote`/`merge` and deletes
its remote-tracking refs, so the branches simply become upstream-less — which is case 4,
not a distinct one.

The case that does exercise the error path is a remote that is still configured but
**unreachable** (server moved, repo deleted, laptop offline). Remote-tracking refs are
local, so the upstream and all merge state survive; only network operations fail. The test
pins that a failed fetch reaches `m.err` and clears `fetching` so `p` can be pressed again,
and that a failed `push --delete` is captured in `remoteErr` and reported on the results
screen without taking the successful local delete down with it.

This also closes Tier 3 item 11 (fetch failure), which shares the shape.

---

## Tier 3 — everyday operations on the happy repo — **DONE 2026-08-17**

### 9. Diverged branch (ahead AND behind) *(covered by `TestDivergedBranch`)*

`trackRe`'s `behind` capture group and `sortAheadBehind` had only ever seen hand-built
structs. Now driven from real `[ahead 2, behind 1]` output, asserting both arrows render and
that a branch ahead of its upstream is not safely deletable.

### 10. Remote delete race *(covered by `TestRemoteDeleteRace`)*

**Correction:** the prediction that this surfaces an error is no longer true, because of the
qualified refspec introduced in the simplify pass (below). `git push origin --delete
refs/heads/x` treats an already-absent ref as a no-op and exits 0, where the bare form fails
with "remote ref does not exist".

That is a deliberate trade: the bare form's error is more informative in this one case, but
the bare form *also silently deletes nothing* when the remote carries a tag of the same name
(git rejects it as "src refspec matches more than one"). Idempotence in the race is worth
more than a message, since the race ends in exactly the state the user armed. The test pins
the idempotent outcome; `TestUnreachableRemote` still covers genuine push failures.

### 11. Fetch failure — **DONE**, see Tier 2 item 8

### 12. Current-branch UI protection *(covered by `TestCurrentBranchCannotBeMarked`)*

`space`, `r`, and `a` all skipping `isCurrent` was only ever implicit in other tests. Now
explicit: the checked-out branch can never be marked or swept into a select-all.

### 13. `loadDiff` HEAD fallback *(covered by `TestLoadDiffFallsBackToHead`)*

Reached with `setupTrunkRepo`, where no remote default and no local `main`/`master` resolve.
Asserts the diff is still correct against HEAD and that the diff header names the base it
actually used.

### 14. Odd-but-legal content *(covered by `TestUnicodeNameAndEmptySubject`)*

An empty commit subject (`--allow-empty-message`) leaves a trailing empty field in the
NUL-delimited `for-each-ref` output — a stricter field count would drop the whole branch.
Combined with a multibyte branch name, pinned end to end through parse, render, and delete.

---

## Tier 4 — git state outside the branch list — **DONE 2026-10-09**

The linked-worktree bug (`bugs.md`, October 5) had one cause: git keeps state outside
`refs/heads` that changes what a delete does, and the tool did not read it. This sweep looked
for more of that shape. Every claim was reproduced in a throwaway repo first.

### 15. Rebase, bisect and `rebase --update-refs` hold branches *(confirmed — fixed)*

git refuses `branch -d` and `-D` on a branch a rebase or bisect started from, in any worktree,
and on every branch a `rebase --update-refs` will move. The worktree's HEAD is detached, so
`%(HEAD)` and `%(worktreepath)` are both empty and the tool showed the branch as free. Fixed by
`loadHeldBranches`, which reads each worktree's `rebase-merge/head-name`,
`rebase-apply/head-name`, `rebase-merge/update-refs` and `BISECT_START`, following git's
`prepare_checked_out_branches`. Held branches show `~` and are locked.
*(`TestHeldBranchesAreLocked`)*

### 16. `r` armed the delete of the wrong remote branch *(confirmed — fixed)*

`r` targets the upstream, which need not share the branch's name. Reproduced:

- `hotfix` made from `origin/release/1.0` with `pruner.protect 'release/*'`: the push deleted
  `release/1.0`. The protect check only looked at the local name.
- `git switch -c feat origin/main`: the push tried to delete `main`. Only the remote's own
  `receive.denyDeleteCurrent` stopped it.
- `--track main` (remote `.`): `remoteName` fell back to `origin`, so the push tried to delete
  `origin`'s `main`, a remote the branch never used.

Fixed by `remoteProtected` (trunk and protect checks on `remoteBranch()`) and
`remoteDeletable()` (now `remoteDelete()`, see 31), which also refuses a local upstream.
*(`TestRemoteDeleteRefusesWrongTargets`)*

### 17. Remote rows ignored fetch refspecs *(confirmed — fixed)*

With `+refs/heads/jb/*:refs/remotes/origin/*`, the row `origin/old` is `jb/old` on the remote,
but the delete pushed `refs/heads/old` and removed a teammate's branch. `mapRemoteRows` now maps
each row back through `remote.<name>.fetch`. Rows that map to no branch, or to more than one
source (the usual pull request refspec next to the default one), are dropped. This fix claimed
that every remote's trunk was protected in the remote view. It was not until item 21.
*(`TestRemoteRowsFollowFetchRefspecs`)*

### 18. Partial clones reach the network from `cherry` and `diff` *(confirmed — fixed)*

In a `--filter=blob:none` clone, `git cherry` fetches blobs when two commits touch the same
files, and `git diff` fetches them for the `v` view. `runGit` treated both as local: no
timeout and no ssh prompt guard. Worse, with the remote unreachable `cherry` exits 128, and
`riskCommitCountRef` read that as **zero** commits at risk, so `x` selected a gone branch whose
unique commit `-D` then discarded. Fixed by `lazyFetch` (set from `remote.*.promisor` or
`extensions.partialClone`), which makes both commands network-bound, and by `riskUnknown`,
which `x`, `p` and the confirm screens treat as risky. *(`TestPartialCloneRiskIsNotGuessed`)*

`git switch` (`c`) also fetches in a partial clone, but stays local: the 60 s timeout would
kill a checkout partway and leave `index.lock` behind.

### Checked, no change needed

- Worktree folder deleted by hand: `git worktree remove --force` still succeeds.
- Bare repo with worktrees: its HEAD branch shows `*` and is locked, though git would delete
  it. The safe side.
- `git am` in progress: HEAD stays on the branch, so it already shows as current.
- Locked worktree: the remove is refused as designed.

## Tier 5 — review of the Tier 4 fixes — **DONE 2026-10-10** (one new item, 34, open)

A `/code-review` of the Tier 4 work found these on 2026-10-09, before its commit. The review
reproduced 19, 21 and 22 in throwaway repos. A read of the code confirmed 19 and 20. Reproduce
each other one before the fix. Every fix gets a test that fails with the fix taken out, then
`go test -race`, `gofmt` and `go vet`, and README and `bugs.md` updates.

### Order

1. **Done.** Commit the Tier 4 work first, so the review fixes have their own diff: `c4846f2`,
   merged in PR #7.
2. **Done.** 31, then 19 inside it: one function, one place for the `r` rules.
3. **Done.** 20, 21, 26, 25: what `r` and the confirm screen protect and show.
4. **Done.** 22, 23, 24: all in `loadHeldBranches`.
5. **Done.** 28, 27: small, separate.
6. **Done.** 29 and 30 together: both split `networkBound`.
7. **Done.** 32 and 33 last: larger, and not wrong today. 33 went first, so 32 carries a
   plain count.

### Correctness

**19. `r` can delete a branch that is not the upstream** — `git.go` `remoteDelete` — **DONE**.
With the pull request refspec listed first, `git switch --track origin/pr/1` gives
`merge = refs/pull/1/head`. `remoteBranch()` then falls back to the tracking name, and the push
deletes `refs/heads/pr/1`, a teammate's branch. Fixed: `r` arms only when `upMerge` starts with
`refs/heads/`, and the refusal names the ref. Reproduced first with git 2.55. Covered by the
`pr/1` case in `TestRemoteDeleteRefusesWrongTargets`: `r` refuses, and the bare remote still has
`refs/heads/pr/1`.

**20. A renaming refspec drops protection** *(regression from 17)* — `model.go` `isProtected` —
**DONE**. `mapRemoteRows` sets `upMerge`, so `remoteBranch()` of the row `origin/release/1.0`
was `jb/release/1.0`, and `release/*` no longer matched. The README says protect globs match
the name shown. Fixed: `protectsRemote` matches the globs against both the shown name and the
remote's name, for remote rows and for `remoteProtected`. Covered in
`TestRemoteRowsFollowFetchRefspecs`: the row `origin/release/1.0` is protected, and `r` refuses
on a local `hotfix` made from `origin/release/2.0`.

**21. Only the default remote's trunk is protected** — `model.go` `protectsName` — **DONE**.
`upstream/master` next to `origin/main` was not protected. `TestRemoteRowsFollowFetchRefspecs`
passed only because `fork`'s trunk was the default remote. Fixed: `resolveBases` keeps the
target of each `refs/remotes/<r>/HEAD` in `m.remoteTrunks`, and `protectsRemote` protects those
refs in both checks. A remote with no `<r>/HEAD` (added with `remote add`, never `set-head`) has
no known trunk. The test now gives `fork` the trunk `master` with `fork/HEAD` set, and gives
`origin` a `main` so `fork` is not the default remote. Item 17's text is corrected.

**22. A repo path with `[`, `*` or `?` hides linked worktrees** — `git.go` `loadHeldBranches` —
**DONE**. `filepath.Glob` read the path as a pattern. Fixed: `os.ReadDir(<common>/worktrees)`.
Covered: `TestHeldBranchesAreLocked` now runs in a folder named `a[1]`.

**23. Left-over bisect state locks a branch git would delete** — `git.go` `readHeld` — **DONE**.
git's `wt_status_check_bisect` reads `BISECT_START` only when `BISECT_LOG` exists. Reproduced
with git 2.55: without `BISECT_LOG`, `git branch -D` succeeds. Fixed: the same check.
*(`TestLeftoverBisectStartDoesNotLock`)*

**24. git older than 2.31 turns off held-branch checks with no error** — `loadHeldBranches` —
**DONE**. Old `rev-parse` prints an unknown `--path-format=absolute` back as output, so `common`
was wrong and every read missed. Checked with git 2.55: `rev-parse` echoes unknown flags, and
prints `--git-common-dir` relative to the current folder (`../../.git` from a subfolder),
or absolute from a linked worktree. Fixed: the flag is gone. `filepath.Abs` is not needed: the
reads resolve the relative path from the same folder. No test can fail without this fix on a
current git. `TestHeldBranchesAreLocked` now also reads the held branches from a subfolder and
from a linked worktree, which guards the relative path.

**25. A stale refusal stays on screen** — `model.go`, the `r` case — **DONE**.
Fixed: `r` clears `m.status` when it toggles. Covered in `TestRemoteDeleteRefusesWrongTargets`:
`r` on a refused row, then on a good row, leaves the status empty.

**26. The confirm screen hides a renamed remote target** — `view.go` `confirmParts` — **DONE**.
For the row `origin/old` mapped to `jb/old`, the confirm screen said only "remote branch — no
local copy", but the result line said `origin/jb/old`. Fixed: it adds
`delete remote <remote>/<remoteBranch()>` when that differs from the row name. Covered in
`TestRemoteRowsFollowFetchRefspecs`: the confirm view contains `origin/jb/old`.

**27. A test check that cannot fail** — `main_test.go`, end of `TestHeldBranchesAreLocked` —
**DONE**. `selectedBranches()` already drops locked rows. Fixed: after `a`, the test checks
`.selected` on each held branch. Mutation check passed: with the locked guard in `a` removed,
the test fails.

**28. A remote name with `/` gives the wrong local name on `c`** — `model.go` `switchCmd` —
**DONE**. Remote `team/a`, row `team/a/x`: the local branch became `a/x`. Fixed:
`strings.TrimPrefix(b.name, b.upRemote+"/")`. *(`TestCheckoutRemoteWithSlashInName`)*

### Design

**29. `git switch` in a partial clone can show an ssh prompt** — `git.go` `networkBound` —
**DONE**. It stays without a timeout on purpose (see 18), so it got no `detachTTY` either.
Fixed: `networkBound` is now `networkUse`, which returns a `gitNet` with three flags:
`connects` (remote slot), `timeout` and `noTTY`. `switch` in a partial clone gets `noTTY` only.

**30. Partial clones run risk counts 3 at a time, not 8** — `networkBound` — **DONE**.
`cherry` and `diff` took the remote slots even when the files were local. Fixed with 29: they
get `timeout` and `noTTY`, and keep the local slots. Both covered in
`TestPartialCloneRiskIsNotGuessed`. Mutation checks passed: `cherry` with `connects`, or
`switch` without `noTTY`, fails it.

**31. `remoteDeletable` and `remoteRefusal` repeat the same checks** — `git.go` — **DONE**.
Fixed: one `remoteDelete() (ok bool, why string)`, done before 19, so 19 added its check once.

**32. Risk counts can freeze the screen** — `model.go` `countRisk` callers — **DONE**.
They ran inside `Update`: in `d`, and in every reload (a fetch for `p`, the end of a delete
run, a switch, an undo, the remote view's load). `x` itself counted nothing. With the 60 s
network timeout from 18, a slow partial clone could block the screen for minutes. Fixed:
- Every reload installs the list and returns `mergeCmd`, the same `tea.Cmd` startup already
  used. `installBranches` sets `m.merging`, so `x` and `d` wait until it lands.
- `p` sets `selectOnMerge`, and `selectAfterFetch` selects the gone branches once the counts
  land.
- `d` calls `confirmAfterRisk`, which counts in a `tea.Cmd` and sets `m.measuring`. The
  `riskMsg` opens the confirm screen. A count for an older load is dropped, with a status
  line that says to press `d` again.
- Script mode and `startDeletions` keep the waiting `measureSelectedRisk`. In the TUI it
  counts nothing, because `d` measured the selection first.

Tests drive the new cmds with a `settle` helper in `press`, `drainDeletions`, `doSwitch` and
`openRemoteView`. *(`TestRiskCountsRunOffTheUpdateLoop`: no git process runs inside `Update`
for a reload or for `d`.)* Mutation checks passed: either path made synchronous again fails it.

**33. `riskUnknown = -1` lives in a count field** — `git.go` — **DONE**.
Any new `> 0` or `== 0` test misread it. Fixed: `riskFailed` sits next to `riskMeasured`, and
`riskCommits` is a plain count. `riskCount{commits, failed}` carries both. Every "commits at
risk" check asks `atRisk()`, which is true for a failed count. Covered in
`TestPartialCloneRiskIsNotGuessed`. Mutation check passed: a plain `riskCommits == 0` on the
confirm screen shows a failed count as clean, and fails the test.

### Found while fixing

**34. A forced retry runs deletes and pushes inside `Update`** — `model.go`
`forceDeleteUnmerged` — **TODO**. `y` on the force prompt runs `git branch -D` and
`pushRemoteDelete` for each refused branch inside `Update`. A push can wait out the 60 s
timeout, so a slow remote freezes the screen, as in 32. Fix: run the retry as a `tea.Cmd`, like
`startDeletions`, and apply the results from a message.

## Ref handling — the rule this work established

**Never use git's `%(refname:short)`, `%(upstream:short)`, or `symbolic-ref --short`.** They
return the shortest *unambiguous* name, which silently grows a `heads/` or `remotes/` prefix
the moment a tag shares the name. Instead:

- Resolvers return **fully qualified refs**. `remoteDefault`, `localDefaultBranch` and
  `baseBranch` all do; resolution is the only point where the namespace is known for
  certain, so it is carried forward from there rather than re-guessed later.
- `shortRef()` shortens at the **display boundary** only. The model keeps both forms:
  `riskBase`/`remoteDefault` for views, `riskBaseRef` for git.
- `branchRef()` qualifies a local branch name. Correct because `loadBranches` only ever
  reads `refs/heads`, so the mapping is total.
- `refExists()` wraps the `rev-parse --verify --quiet` probe and must be given a qualified
  ref — a bare name would match a tag, which is the bug it exists to avoid.

An earlier iteration used a `qualifyRef()` helper that probed both namespaces at the point
of *use*. That was the wrong altitude: it cost up to two subprocesses per call, silently
fell through to the shadowable bare name when both probes missed, and left the real hole
open in `localDefaultBranch`. It has been deleted.

## Status

Tiers 1 to 5 are done. One new item, 34, is open.

The suite has **99 tests** (81 in `main_test.go`, 18 in `features_test.go`), passing under
`-race`, with `gofmt` and `go vet` clean (2026-10-10). Tiers 1 to 3 ended at 43; Tier 4 added 4;
Tier 5 added 3.

- Tier 1 (2026-08-17) fixed two real bugs — tag shadowing on the risk path, and `p` wiping selections.
- Tier 2 found no new bugs; it pins five repo environments that had no coverage, and
  corrected two wrong predictions in this document against real git behavior.
- Tier 3 pins five everyday operations and corrected one more prediction.
- A `/simplify` pass then found **two further live instances of the tag-shadowing bug** that
  Tier 1 had missed (`localDefaultBranch` and `push --delete`), removed the `qualifyRef`
  probe, and brought startup back from 11 git subprocesses to 7 — the pre-change baseline.
  Item 18 in `improvements.md` then took it to 6, in two concurrent rounds rather than six
  serial ones.
- Tier 4 (2026-10-09) fixed four bugs where git state outside the branch list changed what a
  delete does. It is in `c4846f2`, merged in PR #7.

Startup is now 9 git subprocesses in `setupRepo`'s shape, still in two rounds. Later work added
three reads to the first round: the config, the worktree list and the held branches. The first
round is the repo check plus the six reads in `loadRepo`. The second is the two merge queries
against the base. Gone branches add their risk counts on top. `TestMergedBranchesCostNoSubprocess`
caps a load at 12.

Fixtures compose rather than duplicate: `initRepo` (bare init + identity) →
`setupLocalRepo` (branch shapes) → `setupRepo` (+ `addOrigin` + a tracking branch), with
`setupTrunkRepo` reusing `initRepo` and `addOrigin`.
