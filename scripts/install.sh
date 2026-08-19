#!/bin/sh
# Install kopicode (and kopibench) from the latest GitHub Release.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/leejianrong/kopicode/main/install.sh | sh
#
# Plain POSIX sh, not bash: the whole point of a curl-|-sh installer is that it
# runs on whatever shell the user's system default `sh` happens to be, and
# CLAUDE.md's "dependencies stay near-zero" applies here too — no new
# language runtime, no bashisms, just the same subset install-hooks.sh uses.
#
# What this deliberately does NOT do: guess. A repo whose release workflow
# has run zero times, an unsupported OS/arch, or a download that fails all
# fail loudly with a specific message rather than silently doing nothing or
# fabricating a plausible-looking path. See CLAUDE.md's "never fabricate a
# plausible default" discipline (internal/build's version resolution is the
# worked example) — this script is the same discipline applied to an install
# script instead of a Go binary.
set -eu

REPO="leejianrong/kopicode"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"

# kopicode only, by default. Set INSTALL_BIN=kopicode,kopibench to also grab
# the bench runner.
BIN_LIST="${INSTALL_BIN:-kopicode}"

# Matches CLAUDE.md's own assumption about what is already on the user's
# PATH (`~/go/bin` for golangci-lint/gopls/gitleaks) — a user-writable bin
# directory under $HOME rather than a system path that needs sudo.
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

err() {
    echo "install.sh: error: $*" >&2
    exit 1
}

info() {
    echo "install.sh: $*" >&2
}

need_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        err "'$1' is required but not found on PATH"
    fi
}

need_cmd curl

# --- detect platform -------------------------------------------------------
#
# Only linux/darwin, only amd64/arm64: the same PLATFORMS the Makefile
# cross-compiles for, minus windows/amd64, which this curl-|-sh installer is
# explicitly out of scope for (a Windows user is not piping into `sh`).

detect_os() {
    uname_s=$(uname -s)
    case "$uname_s" in
        Linux) echo "linux" ;;
        Darwin) echo "darwin" ;;
        *) err "unsupported OS '$uname_s' (kopicode ships linux and darwin binaries only; build from source: https://github.com/${REPO}#quickstart)" ;;
    esac
}

detect_arch() {
    uname_m=$(uname -m)
    case "$uname_m" in
        x86_64|amd64) echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *) err "unsupported architecture '$uname_m' (kopicode ships amd64 and arm64 binaries only; build from source: https://github.com/${REPO}#quickstart)" ;;
    esac
}

os=$(detect_os)
arch=$(detect_arch)
platform="${os}-${arch}"

info "detected platform: ${platform}"

# --- find the release -------------------------------------------------------
#
# A 404 here means exactly one thing: no tag has ever been pushed, i.e. the
# release workflow (.github/workflows/release.yml, KAN-931) has never fired.
# That is the honest state of this project as of KAN-934, and this script
# says so rather than treating an empty response as "nothing to install" and
# exiting 0. Deliberately no -f: curl's -f discards the response body on an
# HTTP error status, which is exactly the body this script needs to tell a
# "no release yet" 404 apart from a rate-limited or malformed response.
# Connection-level failures (DNS, TLS, timeout) still make curl itself exit
# non-zero, which the `||` below catches.

raw=$(curl -sS -w '\n%{http_code}' "$API_URL") || err "failed to reach the GitHub API at $API_URL"
http_status=$(printf '%s' "$raw" | tail -n1)
release_json=$(printf '%s' "$raw" | sed '$d')

if [ "$http_status" = "404" ]; then
    err "no GitHub release found for ${REPO} yet (HTTP 404 from $API_URL). kopicode has not cut its first release; build from source instead: https://github.com/${REPO}#quickstart"
fi
if [ "$http_status" != "200" ]; then
    err "unexpected HTTP $http_status from $API_URL"
fi

tag=$(printf '%s' "$release_json" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
[ -n "$tag" ] || err "could not parse a release tag from the GitHub API response"
info "latest release: ${tag}"

# --- install each requested binary -----------------------------------------

mkdir -p "$INSTALL_DIR" || err "could not create install directory $INSTALL_DIR"

old_ifs=$IFS
IFS=,
for bin in $BIN_LIST; do
    IFS=$old_ifs
    asset="${bin}-${platform}"
    url="https://github.com/${REPO}/releases/download/${tag}/${asset}"

    # Confirm the asset is actually attached to the release before spending
    # a download on it. release.yml stages one asset per (binary x
    # platform), but a hand-created or partial release could be missing one,
    # and "curl a URL that might 404" deserves a named check, not a bare
    # download whose failure is a stack of curl's own generic errors.
    if ! printf '%s' "$release_json" | grep -q "\"name\": *\"${asset}\""; then
        err "release ${tag} has no asset named '${asset}' for this platform; see https://github.com/${REPO}/releases/tag/${tag}"
    fi

    dest="${INSTALL_DIR}/${bin}"
    tmp="${dest}.download"

    info "downloading ${asset} (${tag}) -> ${dest}"
    if ! curl -fsSL -o "$tmp" "$url"; then
        rm -f "$tmp"
        err "download failed: $url"
    fi

    chmod +x "$tmp" || err "could not make $tmp executable"
    mv "$tmp" "$dest" || err "could not move $tmp to $dest"

    info "installed ${bin} -> ${dest}"
    IFS=,
done
IFS=$old_ifs

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) info "note: $INSTALL_DIR is not on your PATH. Add it, e.g.: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
