#!/usr/bin/env sh
# AgentGate install / upgrade script (deployment pattern follows dsh-ee).
#
# Online install (pulls the GHCR image):
#   curl -fsSL https://raw.githubusercontent.com/myjnathan/AgentGate/main/deploy/install.sh | sh
#   Creates ./agentgate/ (override with AGENTGATE_DIR), generates .env on
#   first run; re-running the script upgrades in place.
#
# Offline install: in an unpacked offline bundle directory, run ./install.sh
# (loads images.tar.gz if present, then starts the bundled compose file).
#
# IMPORTANT: .env contains AGENTGATE_MASTER_KEY. If it is lost, stored
# upstream model keys can no longer be decrypted. Back it up together
# with the data volume.
set -eu

repo="myjnathan/AgentGate"
here="$(dirname "$0")"

if [ -f "$here/VERSION" ] && [ -f "$here/compose.yml" ]; then
  mode=bundle
  cd "$here"
else
  mode=online
  if [ ! -f "$here/compose.yml" ] || [ ! -f "$here/.env" ]; then
    mkdir -p "${AGENTGATE_DIR:-agentgate}"
    cd "${AGENTGATE_DIR:-agentgate}"
  else
    cd "$here"
  fi
  command -v curl >/dev/null || { echo "online install needs curl" >&2; exit 1; }
fi

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "docker compose v2 is required" >&2; exit 1; }

if [ "$mode" = bundle ]; then
  if [ -f images.tar.gz ]; then
    echo "· loading offline images"
    gunzip -c images.tar.gz | docker load
  fi
  export AGENTGATE_IMAGE="agentgate:$(cat VERSION)"
else
  echo "· downloading compose.yml"
  curl -fsSL "https://raw.githubusercontent.com/$repo/main/deploy/compose.yml" -o compose.yml
fi

if [ ! -f .env ]; then
  echo "· generating .env (first install)"
  key="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  {
    echo "AGENTGATE_MASTER_KEY=$key"
    echo "AGENTGATE_PORT=8787"
    echo "# AGENTGATE_DEMO=1   # uncomment for the built-in demo company + mock upstream"
  } > .env
  echo "  master key written to .env — BACK IT UP with your data volume."
fi

echo "· starting AgentGate (docker compose up -d)"
docker compose pull || true
docker compose up -d
echo "✓ AgentGate is up: http://127.0.0.1:$(sed -n 's/^AGENTGATE_PORT=//p' .env | head -1 || echo 8787)"
echo "  re-run this script any time to upgrade."
