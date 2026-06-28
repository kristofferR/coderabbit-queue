#!/usr/bin/env bash
# Unit test for crq acquire(): the distributed-lock steal loop must be bounded and
# must never spin-hang on a stale lock that won't clear.
#
# Regression: a stale lock whose DELETE never sticks (write-API rate limit, perms,
# or a peer recreating it) used to make acquire() `continue` straight back into the
# loop — skipping the backoff sleep and re-stealing with no bound — so `crq enqueue`
# silently hung for minutes. acquire() now caps steals (CRQ_LOCK_MAX_STEALS), always
# backs off, and enforces a wall-clock deadline (CRQ_LOCK_DEADLINE).
#
# acquire() runs in a $(...) subshell in production, so steals are counted via a file
# rather than an in-memory counter (which wouldn't propagate out of the subshell).
set -u
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CRQ="$DIR/crq"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# Pull config defaults + just the acquire() function out of the script (no source guard).
eval "$(grep -E '^CRQ_LOCK_(TRIES|BACKOFF_MAX|MAX_STEALS|DEADLINE|TTL|REF)=' "$CRQ")"
eval "$(awk '/^acquire\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$CRQ")"

CRQ_REPO="owner/repo"; CRQ_HOST="testhost"; EMPTY_TREE="x"
NONCE="11111111-1111-1111-1111-111111111111"
uuidgen() { echo "$NONCE"; }
sleep() { :; }                     # no real waiting
now() { echo 1000000; }            # fixed "now"
iso2epoch() { echo 0; }            # lock commit dated epoch 0 => always stale
_lock_epoch() { echo 0; }
log() { case "$*" in stealing*) echo x >>"$TMP/steals";; esac; printf 'LOG: %s\n' "$*" >&2; }
steal_count() { [ -f "$TMP/steals" ] && wc -l <"$TMP/steals" | tr -d ' ' || echo 0; }

pass=0; fail=0
check() { if [ "$2" = "$3" ]; then echo "  ok: $1 ($2)"; pass=$((pass+1)); else echo "  FAIL: $1 expected=$3 got=$2"; fail=$((fail+1)); fi; }

# ---- S1: stale lock whose DELETE never sticks (the production hang) ----
echo "S1: stale lock that won't delete -> must bail, bounded steals"
: >"$TMP/steals"
_lock_ref_sha() { echo "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"; }   # never freed
_lock_commit_msg() { echo "crq-lock nonce=other epoch=0"; }
gh() { return 0; }   # DELETE "succeeds" but the ref stub still reports it present
start=$(date +%s); out="$(acquire)"; rc=$?; elapsed=$(( $(date +%s) - start ))
check "returns failure" "$rc" "1"
check "no nonce emitted" "${out:-EMPTY}" "EMPTY"
check "steals bounded to CRQ_LOCK_MAX_STEALS" "$(steal_count)" "$CRQ_LOCK_MAX_STEALS"
check "did not hang (<10s, sleep stubbed)" "$([ "$elapsed" -lt 10 ] && echo y || echo n)" "y"

# ---- S2: stale lock; first steal frees it; then acquire ----
# State machine: held(stale) --DELETE--> free("") --create--> our lock(newsha).
echo "S2: steal once -> ref frees -> acquire succeeds"
: >"$TMP/steals"; rm -f "$TMP/freed" "$TMP/created"
_lock_ref_sha() {
  if   [ -f "$TMP/created" ]; then echo "newsha"
  elif [ -f "$TMP/freed" ];   then echo ""
  else echo "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"; fi
}
_lock_commit_msg() { if [ -f "$TMP/created" ]; then echo "crq-lock nonce=$NONCE"; else echo "crq-lock nonce=other epoch=0"; fi; }
gh() { case "$*" in *DELETE*) touch "$TMP/freed"; return 0;; *git/commits*) echo "newsha";; *git/refs*) touch "$TMP/created"; return 0;; esac; return 0; }
out="$(acquire)"; rc=$?
check "returns success" "$rc" "0"
check "emits the nonce" "$out" "$NONCE"
check "exactly one steal" "$(steal_count)" "1"

# ---- S3: lock free from the start ----
echo "S3: free lock -> immediate acquire, zero steals"
: >"$TMP/steals"; rm -f "$TMP/created"
_lock_ref_sha() { if [ -f "$TMP/created" ]; then echo "newsha"; else echo ""; fi; }
_lock_commit_msg() { echo "crq-lock nonce=$NONCE"; }
gh() { case "$*" in *git/commits*) echo "newsha";; *git/refs*) touch "$TMP/created"; return 0;; esac; return 0; }
out="$(acquire)"; rc=$?
check "returns success" "$rc" "0"
check "emits the nonce" "$out" "$NONCE"
check "zero steals" "$(steal_count)" "0"

echo; echo "PASS=$pass FAIL=$fail"; [ "$fail" -eq 0 ]
