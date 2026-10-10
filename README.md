# git_pruner

An interactive terminal UI for pruning local git branches. It lists branches like
`git branch -vv --sort=-committerdate` (most recently committed first), lets you re-sort on the
fly, multi-select branches, and delete them — with a per-branch option to also delete the
matching remote branch.

You can also view a branch's changes as a colorized diff, and fetch-and-prune to find branches
whose upstream has been deleted so they can be cleaned up in one step.

![git_pruner: prune gone branches, arm a remote deletion, confirm, delete](assets/demo.gif)

Above: `p` fetches and prunes, marking three branches whose upstream was deleted and selecting
them; `space` and `r` add a merged branch and arm its remote deletion too; `d` opens the
confirmation, where `R` deletes local **and** remote.

## Install

Requires Go 1.26+ and git on your PATH.

```sh
./install.sh                   # build + install to a user bin directory
./install.sh --bindir ~/bin    # ...or pick the directory yourself
```

With no flags it installs to the first usable of `~/.local/bin` or `~/bin`, falling back to
`/usr/local/bin` (via sudo), and warns if the directory is not on your PATH. See
`./install.sh --help` for the details.

Then run it from inside any git repository:

```sh
git_pruner
```

To print the build's commit and date (from Go's automatic VCS stamping):

```sh
git_pruner version     # also --version, -v
```

## Keybindings

| Key            | Action                                                        |
| -------------- | ------------------------------------------------------------- |
| `↑`/`k`, `↓`/`j` | Move cursor                                                 |
| `g` / `G`      | Jump to top / bottom                                          |
| `space`        | Toggle selection (locked branches cannot be selected — see [Protected branches](#protected-branches)) |
| `a` / `n`      | Select all listed branches / clear selection                 |
| `m`            | Select listed branches that are merged and older than N days (prompts for N) |
| `tab`          | Switch between local branches and [remote-only branches](#remote-only-branches) |
| `c`            | Checkout the branch under the cursor (`git switch`); on a remote-only row, create a local branch that tracks it |
| `/`            | Filter branches by name; `enter` keeps it, `esc` clears it    |
| `r`            | Toggle "also delete remote" for the row (needs an upstream on a remote that is not [protected](#protected-branches)) |
| `v`            | View the branch's diff (through [delta](https://github.com/dandavison/delta) when installed) |
| `x`            | Select gone branches that hold no unique work (no fetch)      |
| `p`            | Fetch `--all --prune`, then select gone branches that hold no unique work |
| `s`            | Cycle sort field: committerdate -> name -> ahead/behind       |
| `o`            | Reverse sort direction                                        |
| `f`            | Toggle delete mode: safe `-d` <-> force `-D`                  |
| `u`            | Undo: recreate the local branches the last delete removed     |
| `d` / `enter`  | Go to the confirmation screen                                 |
| `?`            | Help screen (build metadata, keybindings, column guide)       |
| `q` / `ctrl+c` | Quit                                                          |

On the confirmation screen, `n`/`esc` cancels. When no remote deletions are armed, `y` deletes.
When at least one remote deletion is armed (`R` in the row), the prompt splits so remote
deletion is never a single accidental keystroke: `y` deletes **local branches only** (sparing
the remotes), while `R` deletes **local + remote**. If any branch is then refused by the safe
delete (`-d`) because it isn't fully merged, a follow-up prompt offers to force delete (`-D`)
just those branches — `y` discards their unmerged commits, `n`/`esc` keeps them.

Deletions run in the background with a live progress screen (a spinner plus a per-branch
checklist), so the UI stays responsive while remote pushes complete. `ctrl+c` there asks for a
second press: quitting mid-run can leave a remote branch behind a deleted local one.

On the results screen, `u` recreates the deleted local branches at their old commits, with
their upstream config. `enter` returns to the list, and `q` quits. When you quit, git_pruner
prints a restore command for every branch it deleted this session, so the way back stays in your
scrollback:

```
git_pruner: to restore a deleted branch, run:
  git branch feature/foo 3e2210a…
  git push origin 3e2210a…:refs/heads/feature/foo
```

In the diff view: `↑`/`↓` scroll, `space`/`ctrl+d` page down, `ctrl+u`/`pgup` page up,
`g`/`G` jump to top/bottom, and `q`/`esc`/`v` return to the list.

The confirmation, force-delete, progress and results screens scroll with the same keys, so a
wide selection never pushes the prompt off the bottom of the terminal. The answer keys take
precedence, so `y`, `R` and `n` still work while a list is scrolled.

## Row format

![The branch list: selection, track and merge columns, dates, hashes and subjects](assets/branches.png)

```
> [x] R *  feature/foo        ↑2↓1 ✓  3 days ago   a1b2c3d  Fix the thing
```

- `>` cursor, `[x]` selected, `R` remote deletion armed
- `*` current branch and `P` protected are locked; `+` checked out in another worktree (see
  [Branches checked out in a worktree](#branches-checked-out-in-a-worktree)); `~` held by a
  rebase or bisect in progress (see [Branches held by a rebase or bisect](#branches-held-by-a-rebase-or-bisect))
- ahead/behind shown as `↑N↓M` (`=` when in sync, `gone` in red when the upstream was deleted)
- a green `✓` after the track column means the upstream is merged into the remote default
  branch — i.e. the remote is safe to delete
- relative commit date, short hash, and commit subject. On a narrow terminal the relative date,
  then the hash, then the date drop out, so a row never wraps

## Viewing a branch's changes

Press `v` to see what a branch contains as a colorized patch — **green** for additions, **red**
for removals, magenta hunk headers. The diff is computed against the repository's default branch
(see [Resolving the default branch](#resolving-the-default-branch)) using a three-dot diff
(`git diff <base>...<branch>`), so it shows only the changes introduced on that branch since it
diverged. The view is scrollable for large diffs; the header shows which base it was compared to.

## Resolving the default branch

The default branch is used as the diff base, as the merge target for the `✓` indicator, and to
measure what a force delete would discard. It resolves to `<remote>/HEAD` if set, else
`<remote>/main`, else `<remote>/master`, trying each configured remote in turn with `origin`
first — so repositories whose only remote is named something else (`upstream`, a fork, …) still
get merge information. If no remote resolves, a local `main`/`master` is used.

## Pruning gone branches

Press `p` to run `git fetch --all --prune` in the background (the UI stays responsive). Once it
finishes, any local branch whose upstream was deleted is marked **gone**, and a status line
reports what was found. Press `d` to review and delete them.

If the branches are already marked **gone** (from an earlier fetch), press `x` to select them
without another fetch. Both keys skip gone branches that hold commits not in the base branch;
select those with `space` if you really want to discard them.

This is the interactive equivalent of:

```sh
git fetch --all --prune && git branch -vv | awk '/: gone]/{print $1}' | xargs git branch -D
```

Gone branches are always removed with `git branch -D` (force), since `-d` refuses a branch whose
upstream no longer exists — this is why selecting them via `x`/`p` prunes them even in safe mode.

Because `-D` discards unmerged commits and git reports **no ahead/behind count for a gone
branch**, git_pruner measures each one against the default branch with `git cherry` and counts
the commits that have no equivalent patch there:

- gone branches holding **no** such commits are auto-selected by `p` — the one-keystroke workflow
- gone branches that **do** hold unique commits are left unselected and reported in the status
  line, so discarding them takes a deliberate `space`; the confirmation screen then shows
  `⚠ N commit(s) not in <base> — force delete (-D) will discard them`

`git cherry` is used rather than `git rev-list <base>..<branch>` so commits that were
cherry-picked, rebased, or squashed individually into the base are correctly recognized as
already integrated. A group of commits squashed together into one still counts as unique, since
no single equivalent patch exists — which is why the warning reads "not in `<base>`" rather than
claiming the work is unrecoverable.

## Deletion behavior

- Local: `git branch -d` by default (refuses unmerged branches); `f` switches to `git branch -D`.
  Branches whose upstream is **gone** are always deleted with `-D`, regardless of the mode, and
  the confirmation screen flags any commits that would be discarded (see above).
  When a `-d` delete is refused for being unmerged, a follow-up prompt lets you retry those
  branches with `-D` without leaving the results — no need to back out and re-select.
- Remote: when armed with `r`, runs `git push <remote> --delete <branch>` on the branch's
  upstream, as git records it. That upstream need not share the branch's name: a branch made
  with `git switch -c feat origin/main` tracks `main`. So `r` refuses when the upstream is the
  default branch or protected, and when the upstream is a local branch (`--track main`), which
  has no remote branch to delete. Because this affects shared history, remote deletion
  requires the explicit `R` key on the confirmation screen — plain `y` deletes locals only.
  The confirmation screen also shows, per branch, whether the upstream is merged into the remote
  default (`✓ merged` / `⚠ not merged`) to help you judge whether the remote is safe to delete.
- A confirmation screen always lists exactly what will be deleted before anything happens.
  Deletions then run concurrently in the background on a live progress screen, and a results
  screen reports per-branch success or failure.

## Branches checked out in a worktree

git refuses to delete a branch that any worktree has checked out, even with `-D`. git_pruner
marks such a branch with `+` and runs `git worktree remove --force` before it deletes the
branch. The confirmation screen names each worktree it will remove.

- A worktree is scratch space for its branch. The remove deletes the worktree folder and
  everything in it, uncommitted changes included, in safe and force mode alike.
- A worktree whose folder is already gone is removed too.
- If the safe delete (`-d`) would refuse the branch as not fully merged, the worktree stays
  until you accept the force delete prompt. The branch's commits are the work at risk there.
- git refuses to remove a locked worktree (`git worktree lock`), and the branch stays.
- A branch checked out in the main worktree is locked: git cannot remove the main worktree.
  This happens when you run git_pruner from a linked worktree.
- Script mode removes worktrees the same way.

## Branches held by a rebase or bisect

git also refuses to delete a branch that a rebase or a bisect started from, in any worktree,
even with `-D`. A `git rebase --update-refs` holds every branch in its stack. git_pruner marks
such a branch with `~`, says `rebase in progress` or `bisect in progress` on its row, and locks
it. Finish or abort the operation (`git rebase --abort`, `git bisect reset`) to free it.

## Selecting merged, old branches

Press `m`, type an age in days (it starts at 90, or at `pruner.staleDays`), and press `enter`.
git_pruner selects every listed branch that is merged into the default branch and whose last
commit is older than that. A local branch counts as merged when its tip is in the default
branch, or when its upstream is merged and it has no commits of its own on top. Like `a`, it only
acts on the listed rows, so a `/` filter narrows it.

## Remote-only branches

Press `tab` to list the remote branches that no local branch tracks — work other people pushed,
or branches you deleted locally but not on the remote. They are read on first use, so startup
does not pay for them. A `✓` means the branch is merged into the remote default; the
confirmation screen counts the commits of any branch that is not.

Deleting one is a push, so only `R` does it; `y` never touches a remote. The exit summary
prints the `git push` command that puts a deleted remote branch back.

Each row is mapped back to its branch on the remote through the remote's fetch refspecs
(`remote.<name>.fetch`), so a refspec that renames branches deletes the right one. A row that
maps to something other than one remote branch is not listed: pull request refs fetched with
`+refs/pull/*/head:refs/remotes/origin/pr/*` are not branches a push can delete.

## Protected branches

The default branch is always protected. Add your own with name globs:

```sh
git config --add pruner.protect 'release/*'
git config --add pruner.protect develop
```

A protected branch shows `P` and cannot be selected by any key. Remote rows are matched by the
branch part of their name, so `release/*` covers `origin/release/1.0` too. The same check
applies to the remote branch a local branch tracks: `r` cannot arm the delete of
`origin/release/1.0` from a local `hotfix` made from it.

## Script mode

The same selection rules work without the screen, for cron jobs or shell aliases:

```sh
git_pruner --prune-gone                        # list what would be deleted
git_pruner --fetch --prune-gone --yes          # fetch, then delete
git_pruner --merged-older-than 90 --yes        # delete merged branches older than 90 days
```

Nothing is deleted without `--yes` (`--dry-run` forces a listing even with it). Script mode never
deletes remote branches, and never deletes a gone branch that holds commits missing from the
default branch. It prints restore commands for what it deleted, and exits 1 if any delete failed.

## Network calls

`git fetch` and `git push --delete` run with `GIT_TERMINAL_PROMPT=0` and without a terminal, and
stop after 60 seconds. A remote that asks for a password or does not answer fails with a message,
instead of freezing the screen. Use a credential helper or an ssh agent for remotes that need a
login.

In a partial clone (`git clone --filter=blob:none`), git fetches file contents when it needs
them, so `git cherry` (the commit count) and `git diff` (`v`) can also reach the remote. They get
the same limits there. A commit count that fails is shown as unknown, never as zero, so `x` and
`p` leave that branch unselected.

## Settings

The sort field and order are saved in `~/Library/Application Support/git_pruner/settings`
(macOS) or `~/.config/git_pruner/settings` (Linux), and restored on the next start.

## Development

```sh
make build      # build straight to $BINDIR (default ~/shared/bin; override with BINDIR=...)
make test       # go test ./...
make vet        # go vet ./...
make clean      # remove the binary from $BINDIR
make assets     # re-record the README's demo GIF and screenshot (needs vhs)
```

CI runs `gofmt`, `go build`, `go vet`, and `go test -race` on Linux and macOS for every push to
`master` and every pull request (`.github/workflows/ci.yml`).

The README's `assets/demo.gif` and `assets/branches.png` are generated, not hand-captured — rerun
`make assets` (needs [vhs](https://github.com/charmbracelet/vhs): `brew install vhs`) after any
change to the UI. It builds a throwaway repo under `/tmp/git_pruner-demo` with branches in every
interesting state — merged, stale upstream, unmerged work, no upstream — then records both assets
against that one repo so their commit hashes agree. The two tapes share their terminal settings
and setup via `assets/common.tape`, since the GIF and the still sit side by side in this file and
would look mismatched if the width or theme drifted between them.

[`docs/improvements.md`](docs/improvements.md) records the codebase analysis, the reasoning behind
the current safety behavior, and the roadmap of remaining work.

The code is split by layer: `git.go` (every git call), `model.go` (state and key handling),
`view.go` (rendering), `cli.go` (script mode), `settings.go`, and `main.go`.

The test suite drives a real `git` binary against throwaway repositories created per test, so it
needs `git` on `PATH` and a committer identity (`user.name` / `user.email`); the tests set one
inside each temporary repo.

## License

[MIT](LICENSE)
