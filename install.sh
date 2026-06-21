#!/usr/bin/env bash
# crq installer — fetches the crq CLI into ~/.local/bin (override with CRQ_BIN_DIR).
#   curl -fsSL https://raw.githubusercontent.com/kristofferR/coderabbit-queue/main/install.sh | bash
set -euo pipefail

REPO="${CRQ_INSTALL_REPO:-kristofferR/coderabbit-queue}"
REF="${CRQ_INSTALL_REF:-main}"
BIN_DIR="${CRQ_BIN_DIR:-$HOME/.local/bin}"
SRC="https://raw.githubusercontent.com/${REPO}/${REF}/crq"

say() { printf 'crq-install: %s\n' "$*"; }

command -v gh >/dev/null 2>&1 || say "WARNING: 'gh' (GitHub CLI) not found — crq needs it at runtime."
command -v jq >/dev/null 2>&1 || say "WARNING: 'jq' not found — crq needs it at runtime."

mkdir -p "$BIN_DIR"
say "downloading $SRC"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$SRC" -o "$BIN_DIR/crq"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$BIN_DIR/crq" "$SRC"
else
  say "ERROR: need curl or wget"; exit 1
fi
chmod +x "$BIN_DIR/crq"
say "installed to $BIN_DIR/crq"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "NOTE: $BIN_DIR is not on your PATH — add: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

say "next: export CRQ_REPO=youruser/crq-state && crq init"
