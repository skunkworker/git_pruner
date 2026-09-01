#!/bin/sh
# Build git_pruner and install it onto the user's PATH. Run with --help for flags.
set -eu

BINARY=git_pruner
BINDIR=${BINDIR:-}

usage() {
	cat <<EOF
Usage: ./install.sh [--bindir DIR]

Builds $BINARY and installs it to a directory on your PATH.

Options:
  --bindir DIR   Install into DIR (also settable via the \$BINDIR environment
                 variable). Relative paths resolve against the current
                 directory.
  -h, --help     Show this help.

Without --bindir, an existing $BINARY on PATH is overwritten; otherwise the
first usable of ~/.local/bin or ~/bin wins, falling back to /usr/local/bin —
which needs sudo on most systems.
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	--bindir)
		[ $# -ge 2 ] || { printf '%s\n' "install.sh: --bindir needs a directory" >&2; exit 2; }
		BINDIR=$2
		shift 2
		;;
	--bindir=*)
		BINDIR=${1#--bindir=}
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		printf '%s\n' "install.sh: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

command -v go >/dev/null 2>&1 || {
	printf '%s\n' "install.sh: go is not on PATH (see https://go.dev/dl/)" >&2
	exit 1
}
# git_pruner shells out to git for every operation, so a missing git makes the
# installed binary useless.
command -v git >/dev/null 2>&1 || {
	printf '%s\n' "install.sh: git is not on PATH" >&2
	exit 1
}

# Usable means writable without sudo: the target binary (if it exists) and the
# directory (or nearest existing ancestor) are writable. Clobbers _dir/_parent,
# since POSIX sh has no `local`.
target_is_usable() {
	_target_dir=$1
	_target_file=$_target_dir/$BINARY
	if [ -e "$_target_file" ] && [ ! -w "$_target_file" ]; then
		return 1
	fi
	_dir=$_target_dir
	while [ ! -d "$_dir" ]; do
		_parent=$(dirname "$_dir")
		if [ "$_parent" = "$_dir" ]; then
			return 1 # walked up to / without finding an existing ancestor
		fi
		_dir=$_parent
	done
	[ -w "$_dir" ]
}

# If BINDIR was not explicitly set, check if an existing binary is on PATH so
# we overwrite the existing build.
if [ -z "$BINDIR" ]; then
	if existing=$(command -v "$BINARY" 2>/dev/null) && [ -n "$existing" ]; then
		existing_dir=$(dirname "$existing")
		case $existing_dir in
		/*) ;;
		*) existing_dir=$PWD/$existing_dir ;;
		esac
		repo_dir=$(cd -- "$(dirname -- "$0")" && pwd)
		resolved_existing_dir=$(cd "$existing_dir" 2>/dev/null && pwd || true)
		if [ -n "$resolved_existing_dir" ] && [ "$resolved_existing_dir" != "$repo_dir" ]; then
			BINDIR=$existing_dir
		fi
	fi
fi

# HOME is unset in some headless and sudo environments, and set -u would abort
# on "$HOME"; skipping straight to the fallback is the right answer there.
if [ -z "$BINDIR" ] && [ -n "${HOME:-}" ]; then
	for candidate in "$HOME/.local/bin" "$HOME/bin"; do
		if target_is_usable "$candidate"; then
			BINDIR=$candidate
			break
		fi
	done
fi
# No user-owned directory was usable: fall back to the conventional system
# location and let the sudo path below deal with the permissions.
[ -n "$BINDIR" ] || BINDIR=/usr/local/bin

# Resolve a relative BINDIR against the caller's cwd before the cd below moves us.
case $BINDIR in
/*) ;;
*) BINDIR=$PWD/$BINDIR ;;
esac

# Build from the repo root regardless of the caller's cwd; the build must run
# inside the git checkout for Go's VCS stamping (git_pruner version). CDPATH is
# cleared because an exported one would redirect this cd.
# shellcheck disable=SC1007 # the empty CDPATH= is a deliberate command prefix
CDPATH= cd -- "$(dirname -- "$0")"

tmpdir=$(mktemp -d)
# POSIX signal traps resume execution afterwards, so the signal handlers have to
# exit themselves; EXIT then does the cleanup exactly once.
trap 'rm -rf "$tmpdir"' EXIT
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM

printf '%s\n' "Building $BINARY..."
go build -o "$tmpdir/$BINARY" .

SUDO=
if ! target_is_usable "$BINDIR"; then
	command -v sudo >/dev/null 2>&1 || {
		printf '%s\n' "install.sh: $BINDIR is not writable and sudo is unavailable" >&2
		exit 1
	}
	SUDO=sudo
	printf '%s\n' "$BINDIR needs elevated permissions; using sudo."
fi

# Unquoted on purpose: empty $SUDO must expand to zero words, not an empty one.
$SUDO mkdir -p "$BINDIR"
$SUDO install -m 0755 "$tmpdir/$BINARY" "$BINDIR/$BINARY"

printf '%s\n' "Installed $BINDIR/$BINARY"

case ":$PATH:" in
*":$BINDIR:"*) ;;
*)
	printf '\n%s\n' "Warning: $BINDIR is not on your PATH. Add this to your shell profile:"
	printf '%s\n' "  export PATH=\"$BINDIR:\$PATH\""
	;;
esac
