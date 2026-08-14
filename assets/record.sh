#!/usr/bin/env bash
# Regenerates assets/demo.gif and assets/branches.png. Run after any UI change.
#
# Both tapes record against a single build of the throwaway repo so the commit
# hashes agree between the two assets — the screenshot tape is read-only, the
# demo tape deletes branches, so the order below matters.
set -euo pipefail

cd "$(dirname "$0")/.."
DEMO=/tmp/git_pruner-demo
STILL=assets/branches.png

command -v vhs >/dev/null 2>&1 || {
	printf '%s\n' "record.sh: vhs is not on PATH (brew install vhs)" >&2
	exit 1
}
# The tapes cannot interpolate $DEMO, so they hardcode it; catch the drift.
grep -q "$DEMO" assets/common.tape || {
	printf '%s\n' "record.sh: assets/common.tape no longer refers to $DEMO" >&2
	exit 1
}

./assets/make-demo-repo.sh "$DEMO"
go build -o "$DEMO/bin/git_pruner" .

# vhs has been observed to skip a Screenshot and still exit 0, which would leave
# the previous still in place and pass silently. Force the failure to be visible.
rm -f "$STILL"
vhs assets/screenshot.tape
[ -f "$STILL" ] || {
	printf '%s\n' "record.sh: vhs did not write $STILL" >&2
	exit 1
}

# The capture is true-colour but only ever shows terminal text, so a 256-colour
# palette is visually indistinguishable and about a third of the size.
if command -v magick >/dev/null 2>&1; then
	magick "$STILL" -strip -colors 256 -dither None \
		-define png:compression-level=9 "$STILL"
fi

vhs assets/demo.tape
rm -f "$DEMO/still.gif"
printf '%s\n' "wrote $STILL and assets/demo.gif"
