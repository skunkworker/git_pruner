### October 5
- [x] Git worktrees checked out to other locations are not able to be cleaned up

### October 9
- [x] Branches held by a rebase, bisect or `rebase --update-refs` showed as free, and their delete failed
- [x] `r` could delete the wrong remote branch: a protected or default upstream, or `origin`'s branch for a local upstream
- [x] Remote-only rows ignored fetch refspecs and could delete a teammate's branch of the same short name
- [x] Partial clones: `git cherry` failing offline counted as zero risky commits, so `x` selected unmerged gone branches

### October 10
- [x] `r` on a branch made from a pull request (`merge = refs/pull/1/head`) pushed the delete of a teammate's branch with the tracking name
- [x] A fetch refspec that renames branches (`refs/heads/jb/*:refs/remotes/origin/*`) stopped `pruner.protect` globs from matching the name shown
- [x] Only the default remote's trunk was protected; `upstream/master` next to `origin/main` was not
- [x] The confirm screen did not name the remote branch a renamed remote row deletes
- [x] A refusal from `r` stayed on screen after `r` armed another row
- [x] A repo path with `[`, `*` or `?` hid linked worktrees from the held-branch check
- [x] A `BISECT_START` left behind without `BISECT_LOG` locked a branch git would delete
- [x] git older than 2.31 turned off the held-branch check with no error
- [x] `c` on a remote row whose remote name holds `/` (`team/a/x`) made the local branch `a/x`
- [x] Partial clones: `git switch` could show an ssh prompt over the screen
- [x] Partial clones: commit counts ran 3 at a time (the push cap), not 8
- [x] Commit counts ran inside the screen's update loop, so a slow partial clone froze the screen
