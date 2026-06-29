#!/usr/bin/env bash
# crq installer.
# Installs the Go crq binary into ~/.local/bin by default.
set -euo pipefail

REPO="${CRQ_INSTALL_REPO:-kristofferR/coderabbit-queue}"
REF="${CRQ_INSTALL_REF:-main}"
BIN_DIR="${CRQ_BIN_DIR:-$HOME/.local/bin}"
NAME="crq"

say() { printf 'crq-install: %s\n' "$*"; }

mkdir -p "$BIN_DIR"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
esac

download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    say "ERROR: need curl or wget"
    exit 1
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

asset="crq_${os}_${arch}.tar.gz"
release_url="https://github.com/${REPO}/releases/latest/download/${asset}"
if [ -z "${CRQ_INSTALL_REF:-}" ]; then
  say "trying release asset $release_url"
  if download "$release_url" "$tmp/crq.tgz" 2>/dev/null \
    && tar -xzf "$tmp/crq.tgz" -C "$tmp" 2>/dev/null \
    && [ -f "$tmp/crq" ]; then
    install -m 0755 "$tmp/crq" "$BIN_DIR/$NAME"
    say "installed to $BIN_DIR/$NAME"
    say "run 'crq help' for the agent loop contract; the repo also includes llms.txt"
    exit 0
  fi
  say "release asset unavailable or unusable; falling back to source build"
fi

command -v go >/dev/null 2>&1 || {
  say "ERROR: Go is required for source install fallback"
  exit 1
}

src="https://github.com/${REPO}/archive/${REF}.tar.gz"
say "downloading source $src"
download "$src" "$tmp/src.tgz"
tar -xzf "$tmp/src.tgz" -C "$tmp"
# GitHub archives extract to a single "<repo>-<ref>/" dir; match it without
# hardcoding the repo name so CRQ_INSTALL_REPO forks also work.
src_dir="$(find "$tmp" -mindepth 1 -maxdepth 1 -type d | head -1)"
[ -n "$src_dir" ] || { say "ERROR: source archive layout not recognized"; exit 1; }

say "building crq"
( cd "$src_dir" && go build -trimpath -ldflags "-s -w" -o "$tmp/crq" ./cmd/crq )
install -m 0755 "$tmp/crq" "$BIN_DIR/$NAME"
say "installed to $BIN_DIR/$NAME"
say "run 'crq help' for the agent loop contract; the repo also includes llms.txt"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "NOTE: $BIN_DIR is not on your PATH; add: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac
