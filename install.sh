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
GITHUB_API="https://api.github.com"
API_BASE="${SYNTY_INSTALL_API:-$GITHUB_API}"
DOWNLOAD_BASE="${SYNTY_INSTALL_DOWNLOAD:-https://github.com}"

log()  { printf 'INFO: %s\n' "$1"; }
err()  { printf 'ERROR: %s\n' "$1" >&2; }

# STAGE is the staging directory, cleared however the script exits. It lives beside
# the install target rather than in /tmp so the final move is a same-filesystem
# rename; across filesystems mv degrades to copy+unlink, where an interruption
# leaves a truncated binary at the live path.
STAGE=""
AUTH_CONF=""
RELEASE_BODY=""
cleanup() {
  [[ -n "$STAGE" ]] && rm -rf "$STAGE"
  [[ -n "$AUTH_CONF" ]] && rm -f "$AUTH_CONF"
  [[ -n "$RELEASE_BODY" ]] && rm -f "$RELEASE_BODY"
  return 0
}
trap cleanup EXIT

# A bare `var=$(cmd)` propagates cmd's status, and `set -e` acts on it, so every
# command substitution below is guarded with `|| true` and its result checked
# explicitly. Without that the script dies silently on the ordinary no-token path,
# before any of the messages written for it can print.
auth_token() {
  # Never sent anywhere but the host it belongs to. API_BASE is a test seam, and a token
  # attached to whatever it names turns "can set an environment variable" (a shared
  # container image, a CI job definition) into "has this user's GitHub token", which
  # `gh auth token` would otherwise hand over from a keyring the env-setter cannot read.
  if [[ "$API_BASE" != "$GITHUB_API" && -z "${SYNTY_INSTALL_ALLOW_TOKEN:-}" ]]; then
    return 0
  fi
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
  RELEASE_BODY=$(mktemp "${TMPDIR:-/tmp}/.synty-release-XXXXXX")
  local opts=(-sSL -o "$RELEASE_BODY" -w '%{http_code}')
  [[ -n "$AUTH_CONF" ]] && opts+=(--config "$AUTH_CONF")
  # The status, not curl's exit code, decides what is reported. `curl -f` exits non-zero
  # for every status at or above 400 and for every transport failure alike, so keying on
  # it told a user whose token is fine that it lacks access while the API answered 502
  # or the network was down, and the real cause was never named.
  local status; status=$(curl "${opts[@]}" "${API_BASE}/repos/${REPO}/releases/latest" 2>/dev/null) || true
  [[ "$status" =~ ^[0-9]{3}$ ]] || status="000"
  local fail="could not resolve the latest release of ${REPO}"
  case "$status" in
    200) ;;
    # GitHub answers 404, not 403, for a private repo the caller cannot see, so these mean
    # either no token or a token without access. Telling someone who already exported one
    # to export one sends them to check the thing that is not wrong.
    401|403|404)
      if [[ -n "$AUTH_CONF" ]]; then
        err "${fail}; the token found does not have access to it (HTTP ${status}; check GITHUB_TOKEN / GH_TOKEN, or \`gh auth status\`)"
      elif [[ "$API_BASE" != "$GITHUB_API" && -z "${SYNTY_INSTALL_ALLOW_TOKEN:-}" ]]; then
        err "${fail}; ${API_BASE} answered HTTP ${status}, and a GitHub token is only ever sent to ${GITHUB_API}"
      else
        err "${fail}; no GitHub token found, and the repo is private (set GITHUB_TOKEN or run \`gh auth login\`)"
      fi
      exit 1 ;;
    000) err "${fail}; could not reach ${API_BASE}"; exit 1 ;;
    *) err "${fail}; ${API_BASE} answered HTTP ${status}"; exit 1 ;;
  esac
  # Whitespace goes first and the match is anchored to the key, for the same reason
  # install_binary does it: the API's compact JSON puts the whole payload on one line,
  # where a greedy match takes the last quoted run in the document — the release body,
  # or the final asset's download URL — and hands back a version that is not one. A tag
  # carries no spaces, so nothing this needs is lost with the whitespace.
  VERSION=$(tr -d '[:space:]' < "$RELEASE_BODY" | grep -oE '"tag_name":"[^"]+"' | head -1 \
    | sed -E 's/.*:"(.*)"$/\1/') || true
  VERSION=${VERSION#v}
  if [[ -z "$VERSION" ]]; then
    err "${fail}; the API answered without a tag_name"
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
    # No silent fall-through. detect_platform refuses an OS this does not build for, so
    # reaching here means a platform was added there and not here, and the check before
    # the move would then pass on anything at all.
    *) err "no executable signature is known for $(uname -s)"; exit 1 ;;
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
    # The URL came out of a response body, and the token goes with the request below, so
    # it has to name the API the token was already sent to rather than any host at all.
    [[ "$url" == "${API_BASE}/repos/${REPO}/releases/assets/"* ]] || {
      err "release v${VERSION} names its asset off ${API_BASE}; refusing to send the token there"; exit 1; }
    curl -fsSL --config "$AUTH_CONF" -H "Accept: application/octet-stream" -o "${STAGE}/${file}" "$url"
  else
    curl -fsSL -o "${STAGE}/${file}" "${DOWNLOAD_BASE}/${REPO}/releases/download/v${VERSION}/${file}"
  fi

  command -v unzip >/dev/null 2>&1 || { err "unzip is required"; exit 1; }
  unzip -q "${STAGE}/${file}" -d "$STAGE"
  [[ -f "${STAGE}/${BINARY_NAME}" ]] || { err "${file} contains no ${BINARY_NAME}"; exit 1; }
  check_executable "${STAGE}/${BINARY_NAME}"
  chmod +x "${STAGE}/${BINARY_NAME}"
  # Flushed before the rename, the way selfupdate flushes before its own. The rename is
  # durable ahead of the data it publishes, so a crash inside the writeback window leaves
  # a truncated binary on PATH, and the smoke test below has already run by then. macOS's
  # sync takes no operand, hence the fallback.
  sync "${STAGE}/${BINARY_NAME}" 2>/dev/null || sync
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
