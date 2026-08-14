# Static list-view screenshot for the README. Regenerate with assets/record.sh.
# Read-only: this tape never deletes a branch, so it can share a repo build with
# demo.tape as long as it runs first.

Output "/tmp/git_pruner-demo/still.gif"

# Sized to the list view alone so the still has no dead space below it. Must
# precede the Source: vhs derives the terminal grid from the Set block as a
# whole, and a Height applied after it silently yields the wrong geometry.
Set Height 360

Source "assets/common.tape"

Type "git_pruner"
Enter
Sleep 3s
# `g` first: on launch the cursor sits on whatever branch git listed first.
Type "g"
Sleep 300ms
Type "jjj"
Sleep 500ms
Show
Sleep 300ms
Screenshot "assets/branches.png"
# vhs has been seen to skip a Screenshot and still exit 0, so record.sh checks
# that this file actually appeared rather than trusting the exit status.
Sleep 300ms
