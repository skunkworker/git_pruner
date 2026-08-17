# The README demo. Regenerate with assets/record.sh (not by running vhs directly:
# the tape assumes the throwaway repo and binary that record.sh stages).
# Story: list -> prune stale upstreams -> arm a remote delete -> confirm -> results.

Output "assets/demo.gif"

# Tall enough for the confirmation screen, the longest of the views recorded
# here. Must precede the Source — see the note in screenshot.tape.
Set Height 580
Set TypingSpeed 55ms

Source "assets/common.tape"

# Unlike the screenshot tape, the shell prompt is on camera here.
Type "PS1='$ ' && clear"
Enter
Show

Sleep 800ms
Type "git_pruner"
Sleep 500ms
Enter
Sleep 2.5s

# Move around the list. `g` first: on launch the cursor sits on whatever branch
# git listed first, so jump to the top to start from a known row.
Type "g"
Sleep 700ms
Type "j"
Sleep 400ms
Type "j"
Sleep 400ms
Type "j"
Sleep 1.2s

# Fetch & prune: the three stale upstreams become "gone" and are auto-selected.
Type "p"
Sleep 3s

# Add a merged branch and arm its remote deletion too.
Type "G"
Sleep 700ms
Space
Sleep 800ms
Type "r"
Sleep 1.8s

# Confirmation screen: the prompt splits because a remote delete is armed.
Type "d"
Sleep 3.2s

# R = local + remote.
Type "R"
Sleep 4s

# Rest on the results screen.
Sleep 3s
Type "q"
Sleep 1.5s
