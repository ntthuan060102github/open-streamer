#!/usr/bin/env bash
# Switch the installed open-streamer to a specific release tag.
#
# Downloads the tagged release tarball from GitHub, runs uninstall to clean up
# the current install, then runs install from the new tarball. Data dir
# (/var/lib/open-streamer, holds config + DVR) is preserved across switches.
#
# Usage:
#   sudo ./reinstall.sh v0.0.7      # explicit tag
#   sudo ./reinstall.sh             # prompts interactively
#   sudo ./reinstall.sh -h          # help
#
# Self-contained: no other repo files needed. Safe to scp to a production
# host and run standalone.

set -euo pipefail

REPO="ntt0601zcoder/open-streamer"
GITHUB="https://github.com/${REPO}"

log()  { printf '\033[36m[reinstall]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[31m[error]\033[0m %s\n' "$*" >&2; }

usage() {
  cat <<EOF
usage: $0 [TAG]

Switch the installed open-streamer to a specific release tag.

Arguments:
  TAG    release tag (e.g. v0.0.7). Prompted interactively if omitted.
         The leading "v" is added automatically if missing.

Examples:
  sudo $0 v0.0.7
  sudo $0 0.0.7         # auto-prefixed to v0.0.7
  sudo $0               # interactive prompt

Releases: ${GITHUB}/releases
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

[[ $EUID -eq 0 ]] || { err "must run as root (use sudo)"; exit 1; }
[[ "$(uname -s)" == "Linux" ]] || { err "this installer only supports Linux"; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) err "unsupported arch: $(uname -m)"; exit 1 ;;
esac
OS="linux"

# Pick a downloader once. curl preferred (better progress, fail-on-404).
if command -v curl >/dev/null 2>&1; then
  download() { curl -fL --progress-bar -o "$1" "$2"; }
  download_quiet() { curl -fsSL -o "$1" "$2"; }
elif command -v wget >/dev/null 2>&1; then
  download() { wget --show-progress -qO "$1" "$2"; }
  download_quiet() { wget -qO "$1" "$2"; }
else
  err "need either curl or wget"; exit 1
fi

# Tag from argv or interactive prompt.
TAG="${1:-}"
if [[ -z "$TAG" ]]; then
  if [[ ! -t 0 ]]; then
    err "no tag given and stdin is not a terminal — pass tag as argument"
    usage
    exit 1
  fi
  read -r -p "Release tag (e.g. v0.0.7): " TAG
fi
# Trim whitespace.
TAG="${TAG#"${TAG%%[![:space:]]*}"}"
TAG="${TAG%"${TAG##*[![:space:]]}"}"
[[ -n "$TAG" ]] || { err "tag is required"; exit 1; }
[[ "$TAG" =~ ^v ]] || TAG="v$TAG"

ARCHIVE="open-streamer-${TAG}-${OS}-${ARCH}.tar.gz"
URL="${GITHUB}/releases/download/${TAG}/${ARCHIVE}"
SUMS_URL="${GITHUB}/releases/download/${TAG}/SHA256SUMS"

log "target:  ${TAG} (${OS}/${ARCH})"
log "archive: ${URL}"

WORK="$(mktemp -d -t "open-streamer-${TAG}.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

log "downloading archive..."
if ! download "${WORK}/${ARCHIVE}" "${URL}"; then
  err "download failed — does the release exist?"
  err "  check ${GITHUB}/releases/tag/${TAG}"
  exit 1
fi

# Verify checksum if the release publishes SHA256SUMS (every tagged release does).
log "verifying checksum..."
if download_quiet "${WORK}/SHA256SUMS" "${SUMS_URL}" 2>/dev/null; then
  EXPECTED="$(awk -v f="$ARCHIVE" '$2==f {print $1}' "${WORK}/SHA256SUMS")"
  if [[ -n "$EXPECTED" ]]; then
    ACTUAL="$(sha256sum "${WORK}/${ARCHIVE}" | awk '{print $1}')"
    if [[ "$EXPECTED" != "$ACTUAL" ]]; then
      err "checksum mismatch!"
      err "  expected: $EXPECTED"
      err "  actual:   $ACTUAL"
      exit 1
    fi
    log "checksum OK"
  else
    warn "no entry for ${ARCHIVE} in SHA256SUMS — skipping verification"
  fi
else
  warn "SHA256SUMS not published — skipping verification"
fi

log "extracting..."
tar -xzf "${WORK}/${ARCHIVE}" -C "${WORK}"

# Inner dir name baked into the release workflow: open-streamer-${OS}-${ARCH}
INNER="${WORK}/open-streamer-${OS}-${ARCH}"
if [[ ! -d "$INNER" ]]; then
  # Fallback: pick whichever single dir the tarball produced.
  INNER="$(find "$WORK" -mindepth 1 -maxdepth 1 -type d ! -name 'open-streamer-*.tar*' | head -n1)"
fi
INSTALLER="${INNER}/build/install.sh"
if [[ ! -d "$INNER" || ! -x "$INSTALLER" ]]; then
  err "tarball layout unexpected — install.sh not found"
  err "  searched: ${INSTALLER}"
  exit 1
fi

if [[ -f "${INNER}/VERSION" ]]; then
  log "release metadata:"
  sed 's/^/  /' "${INNER}/VERSION"
fi

log "uninstalling current version..."
"$INSTALLER" uninstall

log "installing ${TAG}..."
"$INSTALLER" install

log "switched to ${TAG}"
log "  status: systemctl status open-streamer"
log "  logs:   journalctl -u open-streamer -f"
