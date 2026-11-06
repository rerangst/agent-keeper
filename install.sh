#!/bin/sh
# agent-keeper installer and updater (Linux, amd64/arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/rerangst/agent-keeper/main/install.sh | sh
#
# Re-running updates. Pin a release with AGENT_KEEPER_VERSION=vX.Y.Z; install a
# locally built binary with: sh install.sh --local ./dist/agent-keeper
set -eu

REPO="rerangst/agent-keeper"
BIN_DIR="${HOME}/.local/bin"
VERSION="${AGENT_KEEPER_VERSION:-latest}"
LOCAL=""

say() { printf 'agent-keeper: %s\n' "$*"; }
die() { printf 'agent-keeper: error: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: install.sh [--local PATH]

Downloads the agent-keeper release binary for this machine, verifies its
checksum, installs it to ~/.local/bin/agent-keeper and runs
`agent-keeper install` (systemd user units, linger, conflict check).

  --local PATH   install a locally built binary instead of downloading
  AGENT_KEEPER_VERSION=vX.Y.Z   pin a release (default: latest)
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--local)
		[ $# -ge 2 ] || die "--local needs a path"
		LOCAL="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown option: $1 (see --help)" ;;
	esac
done

[ "$(uname -s)" = "Linux" ] || die "only Linux is supported"
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported architecture: $(uname -m)" ;;
esac
ASSET="agent-keeper_linux_${ARCH}"

download() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need curl or wget"
	fi
}

sha256() { # file -> hex digest
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "need sha256sum or shasum to verify the download"
	fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

if [ -n "$LOCAL" ]; then
	[ -f "$LOCAL" ] || die "no such file: $LOCAL"
	cp "$LOCAL" "$TMP/$ASSET"
	say "installing local binary $LOCAL"
else
	if [ "$VERSION" = "latest" ]; then
		BASE="https://github.com/${REPO}/releases/latest/download"
	else
		BASE="https://github.com/${REPO}/releases/download/${VERSION}"
	fi
	say "downloading ${ASSET} (${VERSION})"
	download "${BASE}/${ASSET}" "$TMP/$ASSET" || die "download failed: ${BASE}/${ASSET}"
	download "${BASE}/checksums.txt" "$TMP/checksums.txt" || die "download failed: ${BASE}/checksums.txt"
	want="$(awk -v f="$ASSET" '$2 == f || $2 == "*" f { print $1 }' "$TMP/checksums.txt")"
	[ -n "$want" ] || die "${ASSET} not listed in checksums.txt"
	got="$(sha256 "$TMP/$ASSET")"
	[ "$want" = "$got" ] || die "checksum mismatch for ${ASSET} (want ${want}, got ${got})"
	say "checksum ok"
fi

mkdir -p "$BIN_DIR"
chmod 0755 "$TMP/$ASSET"
# Same-filesystem rename keeps the swap atomic; a running daemon keeps the old inode.
cp "$TMP/$ASSET" "$BIN_DIR/.agent-keeper.new"
mv -f "$BIN_DIR/.agent-keeper.new" "$BIN_DIR/agent-keeper"
say "installed $("$BIN_DIR/agent-keeper" version) to $BIN_DIR/agent-keeper"

# exec would skip the EXIT trap, so clean up first.
rm -rf "$TMP"
trap - EXIT INT TERM
exec "$BIN_DIR/agent-keeper" install
