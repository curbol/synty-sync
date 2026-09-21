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
AUTH_CONF=""
cleanup() {
  [[ -n "$STAGE" ]] && rm -rf "$STAGE"
  [[ -n "$AUTH_CONF" ]] && rm -f "$AUTH_CONF"
  return 0
}
trap cleanup EXIT

# A bare `var=$(cmd)` propagates cmd's status, and `set -e` acts on it, so every
# command substitution below is guarded with `|| true` and its result checked
# explicitly. Without that the script dies silently on the ordinary no-token path,
# before any of the messages written for it can print.
auth_token() {
  local token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [[ -z "$token" ]] && command -v gh >/dev/null 2>&1; then
    # Bounded the way selfupdate bounds the same call: `gh auth token` can go to the
    # network to revalidate (an SSO check, a proxy that drops rather than refuses), and
    # an unbounded one hangs this script with no output, since it never returns for
    # `set -e` to act on. macOS ships no timeout(1), so it stays unbounded there rather
    # than making the installer depend on coreutils.
    if command -v timeout >/dev/null 2>&1; then
      token=$(timeout 3 gh auth token 2>/dev/null || true)
    else
      token=$(gh auth token 2>/dev/null || true)
    fi
  fi
  printf '%s' "$token"
}

# ensure_auth_config points AUTH_CONF at a curl config file holding the Authorization
# header, or leaves it empty when no token is available. The token goes in a file
# rather than curl's argv, where any other account on the machine could read it out of
# ps. It sets a global rather than echoing a path because a command substitution would
# create the file in a subshell, leaving the trap nothing to clean up.
ensure_auth_config() {
  [[ -z "$AUTH_CONF" ]] || return 0
  local token; token=$(auth_token) || true
  [[ -n "$token" ]] || return 0
  AUTH_CONF=$(mktemp "${TMPDIR:-/tmp}/.synty-auth-XXXXXX")
  chmod 600 "$AUTH_CONF"
  printf 'header = "Authorization: token %s"\n' "$token" > "$AUTH_CONF"
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
  ensure_auth_config
  local opts=(-fsSL); [[ -n "$AUTH_CONF" ]] && opts+=(--config "$AUTH_CONF")
  # Whitespace goes first and the match is anchored to the key, for the same reason
  # install_binary does it: the API's compact JSON puts the whole payload on one line,
  # where a greedy match takes the last quoted run in the document — the release body,
  # or the final asset's download URL — and hands back a version that is not one. A tag
  # carries no spaces, so nothing this needs is lost with the whitespace.
  VERSION=$(curl "${opts[@]}" "${API_BASE}/repos/${REPO}/releases/latest" \
    | tr -d '[:space:]' | grep -oE '"tag_name":"[^"]+"' | head -1 \
    | sed -E 's/.*:"(.*)"$/\1/') || true
  VERSION=${VERSION#v}
  # GitHub answers 404, not 403, for a private repo the caller cannot see, so an empty
  # result means either no token or a token without access. Telling someone who already
  # exported one to export one sends them to check the thing that is not wrong.
  if [[ -z "$VERSION" ]]; then
    if [[ -n "$AUTH_CONF" ]]; then
      err "could not resolve the latest release of ${REPO}; the token found does not have access to it (check GITHUB_TOKEN / GH_TOKEN, or \`gh auth status\`)"
    else
      err "could not resolve the latest release of ${REPO}; no GitHub token found, and the repo is private (set GITHUB_TOKEN or run \`gh auth login\`)"
    fi
    exit 1
  fi
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
  ensure_auth_config
  local url
  if [[ -n "$AUTH_CONF" ]]; then
    # Private repo: resolve the asset's API URL, then download with the token.
    #
    # One asset object per line, so the id and the name that selects it are matched
    # together rather than by proximity. Searching a window above "name" and taking the
    # nearest id instead depends on "url" preceding "name" inside the object: reorder
    # those two and the window ends at the name while the last id in it belongs to the
    # asset before, which installs the wrong architecture. Both are ELF between
    # linux-intel and linux-arm64, so check_executable passes and the smoke test at the
    # end is the first thing that notices, after the working binary is gone.
    # "uploader" opens its own brace after "name", so the asset's chunk keeps both
    # fields whichever order they come in.
    # Whitespace goes first so the match holds for the API's compact JSON and for a
    # pretty-printed one alike; asset names carry no spaces, so nothing this needs is
    # lost with it.
    url=$(curl -fsSL --config "$AUTH_CONF" "${API_BASE}/repos/${REPO}/releases/tags/v${VERSION}" \
      | tr -d '[:space:]' | tr '{' '\n' | grep -F "\"name\":\"${file}\"" \
      | grep -oE "https?://[^\"]+/releases/assets/[0-9]+" | head -1) || true
    [[ -n "$url" ]] || { err "asset ${file} not found in release v${VERSION}"; exit 1; }
    curl -fsSL --config "$AUTH_CONF" -H "Accept: application/octet-stream" -o "${STAGE}/${file}" "$url"
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
