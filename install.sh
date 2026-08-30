#!/bin/bash
# synty-sync installer. Downloads the latest release binary for your platform into
# ~/.local/bin. The repo is private, so it authenticates with GITHUB_TOKEN, GH_TOKEN,
# or the gh CLI.
#
# Usage:
#   gh api repos/curbol/synty-sync/contents/install.sh --jq .content | base64 -d | bash
set -euo pipefail

REPO="curbol/synty-sync"
BINARY_NAME="synty-sync"
INSTALL_DIR="${HOME}/.local/bin"
# Where releases are read from. Overridable so install_test.go can run the installer
# end to end against a stub instead of the live GitHub.
API_BASE="${SYNTY_INSTALL_API:-https://api.github.com}"
DOWNLOAD_BASE="${SYNTY_INSTALL_DOWNLOAD:-https://github.com}"

log()  { printf 'INFO: %s\n' "$1"; }
err()  { printf 'ERROR: %s\n' "$1" >&2; }

# STAGE is the staging directory, cleared however the script exits. It lives beside
# the install target rather than in /tmp so the final move is a same-filesystem
# rename; across filesystems mv degrades to copy+unlink, where an interruption
# leaves a truncated binary at the live path.
STAGE=""
cleanup() { [[ -n "$STAGE" ]] && rm -rf "$STAGE"; return 0; }
trap cleanup EXIT

# A bare `var=$(cmd)` propagates cmd's status, and `set -e` acts on it, so every
# command substitution below is guarded with `|| true` and its result checked
# explicitly. Without that the script dies silently on the ordinary no-token path,
# before any of the messages written for it can print.
auth_header() {
  local token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [[ -z "$token" ]] && command -v gh >/dev/null 2>&1; then
    token=$(gh auth token 2>/dev/null || true)
  fi
  if [[ -n "$token" ]]; then
    echo "Authorization: token $token"
  fi
}

detect_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin*) os="mac" ;;
    Linux*)  os="linux" ;;
    *) err "unsupported OS $(uname -s); on Windows use the release zip directly"; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch="intel" ;;
    arm64|aarch64) [[ "$os" == "mac" ]] && arch="apple" || arch="arm64" ;;
    *) err "unsupported arch $(uname -m)"; exit 1 ;;
  esac
  PLATFORM="${os}-${arch}"
  log "platform: $PLATFORM"
}

latest_version() {
  local hdr; hdr=$(auth_header) || true
  local opts=(-fsSL); [[ -n "$hdr" ]] && opts+=(-H "$hdr")
  VERSION=$(curl "${opts[@]}" "${API_BASE}/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/') || true
  VERSION=${VERSION#v}
  [[ -n "$VERSION" ]] || { err "could not resolve latest version (private repo needs gh auth or GITHUB_TOKEN)"; exit 1; }
  log "latest version: $VERSION"
}

# check_executable refuses an asset that is not a native binary for this platform,
# the way the in-process updater does before it swaps a working install. A release
# that shipped the wrong artifact would otherwise be chmod +x'd into place.
check_executable() {
  local magic; magic=$(head -c4 "$1" | od -An -tx1 | tr -d ' \n') || true
  case "$(uname -s)" in
    Linux*)
      [[ "$magic" == "7f454c46" ]] || { err "the downloaded file is not a Linux executable"; exit 1; } ;;
    Darwin*)
      case "$magic" in
        cffaedfe|cefaedfe|cafebabe) ;;
        *) err "the downloaded file is not a macOS executable"; exit 1 ;;
      esac ;;
  esac
}

install_binary() {
  local file="${BINARY_NAME}-${VERSION}-${PLATFORM}.zip"
  mkdir -p "$INSTALL_DIR"
  STAGE=$(mktemp -d "${INSTALL_DIR}/.${BINARY_NAME}-install-XXXXXX")
  local hdr; hdr=$(auth_header) || true
  local url
  if [[ -n "$hdr" ]]; then
    # Private repo: resolve the asset's API URL, then download with the token.
    url=$(curl -fsSL -H "$hdr" "${API_BASE}/repos/${REPO}/releases/tags/v${VERSION}" \
      | grep -F -B3 "\"name\": \"${file}\"" | grep -F '"url"' | sed -E 's/.*"url": "([^"]+)".*/\1/') || true
    [[ -n "$url" ]] || { err "asset ${file} not found in release v${VERSION}"; exit 1; }
    curl -fsSL -H "$hdr" -H "Accept: application/octet-stream" -o "${STAGE}/${file}" "$url"
  else
    curl -fsSL -o "${STAGE}/${file}" "${DOWNLOAD_BASE}/${REPO}/releases/download/v${VERSION}/${file}"
  fi

  command -v unzip >/dev/null 2>&1 || { err "unzip is required"; exit 1; }
  unzip -q "${STAGE}/${file}" -d "$STAGE"
  [[ -f "${STAGE}/${BINARY_NAME}" ]] || { err "${file} contains no ${BINARY_NAME}"; exit 1; }
  check_executable "${STAGE}/${BINARY_NAME}"
  chmod +x "${STAGE}/${BINARY_NAME}"
  mv "${STAGE}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
  log "installed to ${INSTALL_DIR}/${BINARY_NAME}"
}

check_path() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) log "note: $INSTALL_DIR is not on your PATH; add: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

detect_platform
latest_version
install_binary
check_path
# The smoke test decides the script's exit status: err alone returns 0, so anything
# piping this installer would read a broken install as a successful one.
"${INSTALL_DIR}/${BINARY_NAME}" version || { err "installed but 'synty-sync version' failed"; exit 1; }
