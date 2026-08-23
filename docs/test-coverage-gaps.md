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

All three tiers are complete (2026-08-17): **14 tests added, 43 total**, passing under
`-race`, with `gofmt` and `go vet` clean.

- Tier 1 fixed two real bugs — tag shadowing on the risk path, and `p` wiping selections.
- Tier 2 found no new bugs; it pins five repo environments that had no coverage, and
  corrected two wrong predictions in this document against real git behavior.
- Tier 3 pins five everyday operations and corrected one more prediction.
- A `/simplify` pass then found **two further live instances of the tag-shadowing bug** that
  Tier 1 had missed (`localDefaultBranch` and `push --delete`), removed the `qualifyRef`
  probe, and brought startup back from 11 git subprocesses to 7 — the pre-change baseline. It is
6 now, in two concurrent rounds rather than six serial ones (see item 18 in `improvements.md`).

Fixtures compose rather than duplicate: `initRepo` (bare init + identity) →
`setupLocalRepo` (branch shapes) → `setupRepo` (+ `addOrigin` + a tracking branch), with
`setupTrunkRepo` reusing `initRepo` and `addOrigin`.
