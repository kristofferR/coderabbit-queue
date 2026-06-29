#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$DIR"

go test ./...
./crq version | grep -q '^crq '
./crq help | grep -q 'crq loop <repo> <pr>'
./crq help | grep -q 'humans and automation'
./crq help | grep -q 'QUEUE WORKFLOWS'
! ./crq help | grep -q 'crq wait <repo> <pr>'
! ./crq help | grep -q 'crq enqueue <repo> <pr>'
! ./crq help | grep -q 'COMPATIBILITY'
./crq help loop | grep -q 'Never post @coderabbitai review directly'
./crq loop --help | grep -q 'Review round primitive for humans and agents'
./crq help feedback | grep -Fq 'findings[]'
./crq help preflight | grep -q 'official local CodeRabbit CLI'
./crq help doctor | grep -q 'JSON readiness report'
# doctor exits 0 when ready and 1 when not (the documented path on a bare CI
# host). Capture both the JSON and the exit code, and assert it is exactly 0 or 1
# — a bare "|| true" would hide a regression that changed the exit contract.
set +e
doctor_json="$(CRQ_CONFIG=/tmp/crq-test-missing-env CRQ_REPO=owner/crq-state CRQ_ISSUE=1 ./crq doctor)"
doctor_rc=$?
set -e
[ "$doctor_rc" -eq 0 ] || [ "$doctor_rc" -eq 1 ] || { echo "crq doctor exited $doctor_rc, expected 0 or 1" >&2; exit 1; }
printf '%s' "$doctor_json" | jq -e '
  .status == "doctor"
  and (.ready | type == "boolean")
  and (.agent_commands | index("crq preflight --type uncommitted") != null)
  and (.agent_commands | index("crq loop <repo> <pr>") != null)
  and (.tools.gh.found | type == "boolean")
  and (.github.authenticated | type == "boolean")
  and (.coderabbit_cli.authenticated | type == "boolean")
' >/dev/null
