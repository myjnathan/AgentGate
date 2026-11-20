#!/usr/bin/env bash
# AgentGate deployment (bare metal / VM). For containers, build the
# binary and use the same environment variables.
set -euo pipefail

export AGENTGATE_ADDR="${AGENTGATE_ADDR:-127.0.0.1:8787}"
export AGENTGATE_DATA="${AGENTGATE_DATA:-/var/lib/agentgate}"
: "${AGENTGATE_MASTER_KEY:?set AGENTGATE_MASTER_KEY (>=16 chars, store it in your KMS/secret manager)}"

mkdir -p "$AGENTGATE_DATA"
exec "$(dirname "$0")/../agentgate"
