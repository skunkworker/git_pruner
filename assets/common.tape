# Shared by demo.tape and screenshot.tape via `Source`. The GIF and the PNG sit
# next to each other in the README, so the geometry and theme below have to
# agree between them — that is why they live here rather than in each tape.
# Height is deliberately NOT set here: it is the one setting the two recordings
# genuinely differ on.
#
# Sourced paths resolve against vhs's cwd, which record.sh pins to the repo root.

Set Shell "bash"
Set FontSize 15
Set Width 1180
Set Padding 24
Set Theme "Catppuccin Mocha"

# Stage the throwaway repo and binary that record.sh built. Kept hidden: the
# tapes Show once the TUI is on screen. This path is the contract with
# record.sh's $DEMO, which checks that the two still agree.
Hide
Type "export PATH=/tmp/git_pruner-demo/bin:$PATH"
Enter
Type "cd /tmp/git_pruner-demo/orbital"
Enter
