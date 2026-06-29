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
CRQ_CONFIG=/tmp/crq-test-missing-env CRQ_REPO=owner/crq-state CRQ_ISSUE=1 ./crq doctor | jq -e '
  .status == "doctor"
  and (.ready | type == "boolean")
  and (.agent_commands | index("crq preflight --type uncommitted") != null)
  and (.agent_commands | index("crq loop <repo> <pr>") != null)
  and (.tools.gh.found | type == "boolean")
  and (.github.authenticated | type == "boolean")
  and (.coderabbit_cli.authenticated | type == "boolean")
' >/dev/null
