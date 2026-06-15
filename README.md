# git_pruner

An interactive terminal UI for pruning local git branches. It lists branches like
`git branch -vv --sort=-committerdate` (most recently committed first), lets you re-sort on the
fly, multi-select branches, and delete them — with a per-branch option to also delete the
matching remote branch.

## Install

Requires Go 1.26+ and git on your PATH.

```sh
make build      # builds to ~/shared/bin/git_pruner (on PATH)
```

Then run it from inside any git repository:

```sh
git_pruner
```

## Keybindings

| Key            | Action                                                        |
| -------------- | ------------------------------------------------------------- |
| `↑`/`k`, `↓`/`j` | Move cursor                                                 |
| `g` / `G`      | Jump to top / bottom                                          |
| `space`        | Toggle selection (the current branch cannot be selected)     |
| `a` / `n`      | Select all / clear selection                                 |
| `r`            | Toggle "also delete remote" for the row (needs an upstream)   |
| `s`            | Cycle sort field: committerdate -> name -> ahead/behind       |
| `o`            | Reverse sort direction                                        |
| `f`            | Toggle delete mode: safe `-d` <-> force `-D`                  |
| `d` / `enter`  | Go to the confirmation screen                                 |
| `q` / `ctrl+c` | Quit                                                          |

On the confirmation screen, `y` deletes and `n`/`esc` cancels.

## Row format

```
> [x] R *  feature/foo        ↑2↓1   3 days ago   a1b2c3d  Fix the thing
```

- `>` cursor, `[x]` selected, `R` remote deletion armed, `*` current branch
- ahead/behind shown as `↑N↓M` (`=` when in sync, `gone` in red when the upstream was deleted)
- relative commit date, short hash, and commit subject

## Deletion behavior

- Local: `git branch -d` by default (refuses unmerged branches); `f` switches to `git branch -D`.
- Remote: when armed with `r`, runs `git push <remote> --delete <branch>`, where the remote is
  derived from the branch's upstream.
- A confirmation screen always lists exactly what will be deleted before anything happens, and a
  results screen reports per-branch success or failure.

## Development

```sh
make test       # go test ./...
make vet        # go vet ./...
make clean      # remove the installed binary
```
