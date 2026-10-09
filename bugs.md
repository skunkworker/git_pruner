### October 5
- [x] Git worktrees checked out to other locations are not able to be cleaned up

### October 9
- [x] Branches held by a rebase, bisect or `rebase --update-refs` showed as free, and their delete failed
- [x] `r` could delete the wrong remote branch: a protected or default upstream, or `origin`'s branch for a local upstream
- [x] Remote-only rows ignored fetch refspecs and could delete a teammate's branch of the same short name
- [x] Partial clones: `git cherry` failing offline counted as zero risky commits, so `x` selected unmerged gone branches
