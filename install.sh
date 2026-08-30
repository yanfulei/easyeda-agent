#!/usr/bin/env bash
# easyeda-agent installer
# Usage: curl -fsSL https://raw.githubusercontent.com/yanfulei/easyeda-agent/main/install.sh | sh
set -euo pipefail

REPO="${EASYEDA_RELEASE_REPO:-yanfulei/easyeda-agent}"
printf '%s' "$REPO" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' \
  || { printf 'invalid EASYEDA_RELEASE_REPO: %s\n' "$REPO" >&2; exit 1; }
SKILL_NAME="easyeda-agent"
# EASYEDA_INSTALL_SKILLS: ""|auto (detect), "none" (skip), or CSV of codex,claude
INSTALL_SKILLS="${EASYEDA_INSTALL_SKILLS:-}"
# EASYEDA_SKILL_PRESERVE=1 keeps existing files instead of clean-replacing
SKILL_PRESERVE="${EASYEDA_SKILL_PRESERVE:-0}"
# EASYEDA_INSTALL_MCP: auto (register when Codex exists), codex (require it), none
INSTALL_MCP="${EASYEDA_INSTALL_MCP:-auto}"
# Stable user-owned location for the release MCP bundle and its locked dependencies.
MCP_DIR="${EASYEDA_MCP_DIR:-${HOME}/.local/share/easyeda-agent/mcp}"
# EASYEDA_VERSION=v0.18.2 pins the release and skips the GitHub API lookup entirely
VERSION="${EASYEDA_VERSION:-}"

# ── helpers ──────────────────────────────────────────────────────────────────
info()  { printf '\033[34m[easyeda-agent]\033[0m %s\n' "$*"; }
ok()    { printf '\033[32m✔\033[0m %s\n' "$*"; }
warn()  { printf '\033[33m⚠\033[0m %s\n' "$*"; }
fatal() { printf '\033[31m✘\033[0m %s\n' "$*" >&2; exit 1; }
CURL_RETRY=(--connect-timeout 10 --retry 3 --retry-delay 1)

# ── resolve latest release ───────────────────────────────────────────────────
# api.github.com allows only 60 requests/hour per IP unauthenticated, so a shared
# office / NAT / CI address can hand back 403 instead of the release JSON. Send a
# token when we can find one (GITHUB_TOKEN / GH_TOKEN / the gh CLI), and let
# EASYEDA_VERSION bypass the API completely.
github_token() {
  _tok="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [ -z "$_tok" ] && command -v gh >/dev/null 2>&1; then
    _tok=$(gh auth token 2>/dev/null || true)
  fi
  printf '%s' "$_tok"
}

rate_limit_fatal() {
  printf '\033[31m✘\033[0m %s\n' \
    "GitHub API rate limit (HTTP ${1}) — could not resolve the latest release." >&2
  printf '    Unauthenticated api.github.com allows 60 requests/hour per IP;\n' >&2
  printf '    a shared office / NAT / CI address burns through that fast.\n' >&2
  if [ -n "$2" ]; then
    printf '    A token was sent but still rejected — it may be expired or invalid.\n' >&2
  fi
  printf '    Fix it either way:\n' >&2
  printf '      1) authenticate (5000 requests/hour):\n' >&2
  printf '           export GITHUB_TOKEN=<token>    # GH_TOKEN works too\n' >&2
  printf '           gh auth login                 # gh CLI is picked up automatically\n' >&2
  printf '      2) skip the API by pinning a release tag:\n' >&2
  printf '           EASYEDA_VERSION=<tag> sh install.sh\n' >&2
  printf '           tags: https://github.com/%s/releases\n' "$REPO" >&2
  exit 1
}

if [ -n "$VERSION" ]; then
  # Tags are v-prefixed; accept "0.18.2" as well as "v0.18.2".
  case "$VERSION" in
    [0-9]*) VERSION="v${VERSION}" ;;
  esac
  info "Pinned release: ${VERSION} (EASYEDA_VERSION)"
else
  info "Fetching latest release..."
  API_TOKEN=$(github_token)
  API_URL="https://api.github.com/repos/${REPO}/releases/latest"
  # No -f here: we want the body *and* the status code so the failure can explain itself.
  if [ -n "$API_TOKEN" ]; then
    API_RESP=$(curl "${CURL_RETRY[@]}" -sSL -w '\n%{http_code}' \
      -H 'Accept: application/vnd.github+json' \
      -H "Authorization: Bearer ${API_TOKEN}" "$API_URL") || API_RESP=""
  else
    API_RESP=$(curl "${CURL_RETRY[@]}" -sSL -w '\n%{http_code}' \
      -H 'Accept: application/vnd.github+json' "$API_URL") || API_RESP=""
  fi
  API_CODE=$(printf '%s\n' "$API_RESP" | tail -n 1)
  API_BODY=$(printf '%s\n' "$API_RESP" | sed '$d')

  case "$API_CODE" in
    200) ;;
    401) fatal "GitHub API rejected the token (HTTP 401). Unset GITHUB_TOKEN/GH_TOKEN or run 'gh auth login', or pass EASYEDA_VERSION=<tag>." ;;
    403|429) rate_limit_fatal "$API_CODE" "$API_TOKEN" ;;
    404) fatal "No 'latest' release for ${REPO} (HTTP 404). Pick a tag from https://github.com/${REPO}/releases and pass EASYEDA_VERSION=<tag>." ;;
    '' | 000) fatal "Could not reach api.github.com (network or proxy issue). Retry, or pass EASYEDA_VERSION=<tag> to skip the API." ;;
    *) fatal "GitHub API returned HTTP ${API_CODE} while resolving the latest release. Pass EASYEDA_VERSION=<tag> to skip the API." ;;
  esac

  VERSION=$(printf '%s\n' "$API_BODY" \
    | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
  [ -n "$VERSION" ] || fatal "Could not parse a tag_name out of the GitHub API response. Pass EASYEDA_VERSION=<tag> to skip the API."
  info "Latest: ${VERSION}"
fi
printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' \
  || fatal "Release tag must be vMAJOR.MINOR.PATCH (got ${VERSION})"

BASE_URL="${EASYEDA_RELEASE_BASE_URL:-https://github.com/${REPO}/releases/download/${VERSION}}"
case "$BASE_URL" in
  http://*|https://*) ;;
  *) fatal "EASYEDA_RELEASE_BASE_URL must be an http(s) URL (got ${BASE_URL})" ;;
esac

# ── detect OS + arch ─────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)       ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) fatal "Unsupported architecture: $ARCH" ;;
esac

case "$OS" in
  darwin|linux) ;;
  *) fatal "Unsupported OS: $OS (Windows: download easyeda_windows_amd64.exe manually)" ;;
esac

BINARY_NAME="easyeda_${OS}_${ARCH}"

# ── choose install dir (no sudo required) ────────────────────────────────────
if [ -n "${EASYEDA_INSTALL_DIR:-}" ]; then
  INSTALL_DIR="$EASYEDA_INSTALL_DIR"
  mkdir -p "$INSTALL_DIR"
elif [ -w "/usr/local/bin" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "$INSTALL_DIR"
fi
case "$INSTALL_DIR" in
  ""|/|"$HOME") fatal "refusing unsafe EASYEDA_INSTALL_DIR: ${INSTALL_DIR}" ;;
  /*) ;;
  *) fatal "EASYEDA_INSTALL_DIR must be absolute (got ${INSTALL_DIR})" ;;
esac

# ── install CLI binary ────────────────────────────────────────────────────────
info "Downloading ${BINARY_NAME}..."
BIN_TMP="${INSTALL_DIR}/.easyeda-download.$$"
curl "${CURL_RETRY[@]}" -fsSL "${BASE_URL}/${BINARY_NAME}" -o "$BIN_TMP" \
  || { rm -f "$BIN_TMP"; fatal "download failed: ${BASE_URL}/${BINARY_NAME}"; }

# sha256 verification. Old releases may not have checksums.txt, but every asset
# in current releases does. A present checksum mismatch is always fatal.
SHA_TOOL=""
if command -v sha256sum >/dev/null 2>&1; then
  SHA_TOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA_TOOL="shasum"
fi

SUMS=""
if [ -n "$SHA_TOOL" ]; then
  SUMS=$(curl "${CURL_RETRY[@]}" -fsSL "${BASE_URL}/checksums.txt" 2>/dev/null || true)
fi

asset_sha256() {
  if [ "$SHA_TOOL" = "sha256sum" ]; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

verify_asset() {
  _path="$1"; _name="$2"
  if [ -z "$SHA_TOOL" ] || [ -z "$SUMS" ]; then
    warn "sha256 verification skipped for ${_name} (no checksums.txt or sha256 tool)"
    return 0
  fi
  WANT=$(printf '%s\n' "$SUMS" | awk -v n="$_name" '{ f=$2; sub(/^\*/,"",f); if (f==n) { print $1; exit } }')
  GOT=$(asset_sha256 "$_path")
  if [ -z "$WANT" ]; then
    warn "checksums.txt has no entry for ${_name} — skipping verification"
  elif [ "$WANT" != "$GOT" ]; then
    rm -f "$_path"
    fatal "checksum mismatch for ${_name} (want ${WANT}, got ${GOT})"
  else
    ok "sha256 verified: ${_name}"
  fi
}

verify_asset "$BIN_TMP" "$BINARY_NAME"

chmod +x "$BIN_TMP"
mv "$BIN_TMP" "${INSTALL_DIR}/easyeda"
ok "CLI installed → ${INSTALL_DIR}/easyeda"

# ── install skills (Codex + Claude Code) ──────────────────────────────────────
# Resolve which clients to install for.
# codex → ~/.codex/skills/easyeda-agent, claude → ~/.claude/skills/easyeda-agent
detect_targets() {
  # Explicit "none" → skip entirely.
  case "$INSTALL_SKILLS" in
    none|NONE|None) return 0 ;;
  esac

  if [ -n "$INSTALL_SKILLS" ] && [ "$INSTALL_SKILLS" != "auto" ]; then
    # Explicit CSV list (e.g. "codex,claude").
    printf '%s\n' "$INSTALL_SKILLS" | tr ',' '\n' | while IFS= read -r t; do
      t=$(printf '%s' "$t" | tr -d '[:space:]')
      [ -n "$t" ] && printf '%s\n' "$t"
    done
    return 0
  fi

  # auto-detect
  found=0
  if [ -d "${HOME}/.codex" ] || command -v codex >/dev/null 2>&1; then
    printf 'codex\n'; found=1
  fi
  if [ -d "${HOME}/.claude" ] || command -v claude >/dev/null 2>&1; then
    printf 'claude\n'; found=1
  fi
  # Neither detected → create both by default so the skill is ready when a
  # client shows up. EASYEDA_INSTALL_SKILLS=none opts out.
  if [ "$found" = 0 ]; then
    warn "No Codex/Claude Code client detected; creating both skill dirs by default." >&2
    printf 'codex\n'
    printf 'claude\n'
  fi
}

# Map a client name to its skills base dir.
client_base_dir() {
  case "$1" in
    codex)  printf '%s/.codex/skills\n' "$HOME" ;;
    claude) printf '%s/.claude/skills\n' "$HOME" ;;
    *)      return 1 ;;
  esac
}

# install_skill_to <client> <src_skill_dir>
# Cleanly replaces <base>/easyeda-agent from the release (no backup to avoid polluting the skills dir).
install_skill_to() {
  _client="$1"; _src="$2"
  _base=$(client_base_dir "$_client") || { warn "Unknown skill target: ${_client} (skipped)"; return 0; }
  mkdir -p "$_base"
  _dest="${_base}/${SKILL_NAME}"

  # Records the installed version so the daemon's startup skill-sync knows this
  # dir is already current and skips a needless re-download (see `easyeda skill`).
  _write_marker() { printf '%s\n' "${VERSION#v}" > "${_dest}/.version"; }

  if [ ! -d "$_dest" ]; then
    cp -r "$_src" "$_dest"
    _write_marker
    ok "${_client} skill installed → ${_dest}"
    return 0
  fi

  if [ "$SKILL_PRESERVE" = "1" ]; then
    cp -rn "$_src"/. "$_dest"/ 2>/dev/null || cp -r "$_src"/. "$_dest"/
    _write_marker
    ok "${_client} skill updated (preserve mode, existing files kept) → ${_dest}"
    return 0
  fi

  # Detect local modifications vs the release; clean-replace if different.
  if diff -r "$_src" "$_dest" >/dev/null 2>&1; then
    _write_marker
    ok "${_client} skill already up to date → ${_dest}"
    return 0
  fi
  rm -rf "$_dest"
  cp -r "$_src" "$_dest"
  _write_marker
  ok "${_client} skill updated → ${_dest}"
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

TARGETS=$(detect_targets)
if [ -z "$TARGETS" ]; then
  info "Skill install skipped (EASYEDA_INSTALL_SKILLS=none)"
else
  info "Downloading skills.tar.gz..."
  SKILL_ARCHIVE="${TMP}/skills.tar.gz"
  curl "${CURL_RETRY[@]}" -fsSL "${BASE_URL}/skills.tar.gz" -o "$SKILL_ARCHIVE" \
    || fatal "download failed: ${BASE_URL}/skills.tar.gz"
  verify_asset "$SKILL_ARCHIVE" "skills.tar.gz"
  tar -xzf "$SKILL_ARCHIVE" -C "$TMP"
  SRC_SKILL="${TMP}/${SKILL_NAME}"
  [ -d "$SRC_SKILL" ] || fatal "skills.tar.gz did not contain ${SKILL_NAME}/"
  printf '%s\n' "$TARGETS" | while IFS= read -r client; do
    [ -n "$client" ] && install_skill_to "$client" "$SRC_SKILL"
  done
fi

# ── install + register MCP for Codex ─────────────────────────────────────────
# `codex mcp add` replaces an existing entry with the same name, so rerunning the
# installer upgrades both the files and registration without duplicate servers.
MCP_REQUIRED=0
case "$INSTALL_MCP" in
  auto)
    if ! command -v codex >/dev/null 2>&1; then
      info "MCP install skipped (Codex not detected; EASYEDA_INSTALL_MCP=codex forces it)"
      INSTALL_MCP="none"
    fi
    ;;
  codex) MCP_REQUIRED=1 ;;
  none|NONE|None) INSTALL_MCP="none" ;;
  *) fatal "EASYEDA_INSTALL_MCP must be auto, codex, or none (got ${INSTALL_MCP})" ;;
esac

if [ "$INSTALL_MCP" != "none" ]; then
  if ! command -v codex >/dev/null 2>&1; then
    fatal "EASYEDA_INSTALL_MCP=codex but codex is not in PATH"
  fi
  if ! command -v node >/dev/null 2>&1 \
    || ! node -e 'const [a,b]=process.versions.node.split(".").map(Number); process.exit(a>20||(a===20&&b>=17)?0:1)'; then
    if [ "$MCP_REQUIRED" = 1 ]; then
      fatal "MCP requires Node.js >=20.17.0"
    fi
    warn "Codex detected, but Node.js >=20.17.0 is unavailable; MCP install skipped"
    INSTALL_MCP="none"
  fi
fi

if [ "$INSTALL_MCP" != "none" ]; then
  case "$MCP_DIR" in
    ""|/|"$HOME"|"$HOME/") fatal "refusing unsafe EASYEDA_MCP_DIR: ${MCP_DIR}" ;;
    /*) ;;
    *) fatal "EASYEDA_MCP_DIR must be absolute (got ${MCP_DIR})" ;;
  esac
  info "Downloading mcp.tar.gz..."
  MCP_ARCHIVE="${TMP}/mcp.tar.gz"
  curl "${CURL_RETRY[@]}" -fsSL "${BASE_URL}/mcp.tar.gz" -o "$MCP_ARCHIVE" \
    || fatal "download failed: ${BASE_URL}/mcp.tar.gz"
  verify_asset "$MCP_ARCHIVE" "mcp.tar.gz"

  MCP_UNPACK="${TMP}/mcp-unpack"
  mkdir -p "$MCP_UNPACK"
  tar -xzf "$MCP_ARCHIVE" -C "$MCP_UNPACK"
  SRC_MCP="${MCP_UNPACK}/mcp"
  [ -f "${SRC_MCP}/src/server.mjs" ] \
    || fatal "mcp.tar.gz did not contain mcp/src/server.mjs"
  [ -d "${SRC_MCP}/node_modules/@modelcontextprotocol/sdk" ] \
    || fatal "mcp.tar.gz did not contain locked MCP production dependencies"
  node --check "${SRC_MCP}/src/server.mjs"

  MCP_PARENT=$(dirname "$MCP_DIR")
  MCP_STAGE="${MCP_PARENT}/.easyeda-agent-mcp.new.$$"
  MCP_BACKUP="${MCP_PARENT}/.easyeda-agent-mcp.old.$$"
  mkdir -p "$MCP_PARENT"
  rm -rf "$MCP_STAGE" "$MCP_BACKUP"
  cp -R "$SRC_MCP" "$MCP_STAGE"
  printf '%s\n' "${VERSION#v}" > "${MCP_STAGE}/.version"
  if [ -e "$MCP_DIR" ] || [ -L "$MCP_DIR" ]; then
    mv "$MCP_DIR" "$MCP_BACKUP"
  fi
  if mv "$MCP_STAGE" "$MCP_DIR"; then
    rm -rf "$MCP_BACKUP"
  else
    [ ! -e "$MCP_BACKUP" ] || mv "$MCP_BACKUP" "$MCP_DIR"
    fatal "could not install MCP bundle to ${MCP_DIR}"
  fi
  ok "MCP bundle installed → ${MCP_DIR}"

  CODEX_BIN=$(command -v codex)
  NODE_BIN=$(command -v node)
  if "$CODEX_BIN" mcp add easyeda-agent \
      --env "EASYEDA_BIN=${INSTALL_DIR}/easyeda" \
      -- "$NODE_BIN" "${MCP_DIR}/src/server.mjs" \
      && "$CODEX_BIN" mcp get easyeda-agent --json >/dev/null; then
    ok "Codex MCP registered → easyeda-agent"
  elif [ "$MCP_REQUIRED" = 1 ]; then
    fatal "MCP files installed, but Codex registration failed"
  else
    warn "MCP files installed, but Codex registration failed; retry with EASYEDA_INSTALL_MCP=codex"
  fi
fi

# ── PATH check ────────────────────────────────────────────────────────────────
if ! echo ":${PATH}:" | grep -q ":${INSTALL_DIR}:"; then
  warn "${INSTALL_DIR} is not in PATH"
  printf '    Add to ~/.zshrc or ~/.bashrc:\n'
  printf '    export PATH="$PATH:%s"\n\n' "$INSTALL_DIR"
fi

# ── next steps ────────────────────────────────────────────────────────────────
printf '\n'
ok "easyeda-agent ${VERSION} installed"
printf '\n'
printf 'Next steps:\n'
printf '  1. Start the daemon:\n'
printf '       easyeda daemon start\n\n'
printf '  2. Install the EasyEDA connector extension (either channel):\n'
printf '     a) Sideload this release (strictly CLI-version-locked, recommended):\n'
printf '          Download: %s/easyeda-agent-connector.eext\n' "$BASE_URL"
printf '          In EasyEDA Pro: 扩展管理 → 导入扩展 → select the .eext file\n'
printf '     b) 立创官方插件市场 (one-click, auto-updates in place; may lag the CLI):\n'
printf '          https://jlc-ext.com/item/zhoushoujian/easyeda-agent-connector\n'
printf '          (renamed from easyeda-agent-connector; same uuid — existing installs\n'
printf '           keep auto-updating in place, no action needed)\n\n'
printf '  3. In EasyEDA Pro: 设置 → 允许外部交互 (Allow external interaction)\n\n'
printf '  4. Use the skill in your AI client:\n'
printf '       /easyeda-agent       (schematic + PCB workflow)\n'
printf '       Installed for detected clients: Codex (~/.codex/skills) and/or Claude Code (~/.claude/skills)\n\n'
if [ "$INSTALL_MCP" != "none" ]; then
  printf '     Codex MCP: easyeda-agent (registered; start a new Codex session to discover it)\n\n'
fi
printf 'Full-stack upgrade (CLI + Skill + MCP) is the same idempotent install command.\n'
printf 'For a lighter CLI + Skill-only update:\n'
printf '       easyeda update           # CLI binary + skill dirs → latest\n'
printf '       easyeda update --check   # report only (cli / skill / connector)\n'
printf '     (the connector .eext still needs a manual re-import — `update` prints the URL)\n\n'
printf 'Full docs: https://github.com/%s\n' "$REPO"
