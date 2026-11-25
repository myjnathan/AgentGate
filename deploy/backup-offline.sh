#!/usr/bin/env bash
# AgentGate offline backup (backlog F27 phase 3 A): copies the live
# data directory into a timestamped, checksummed directory — the
# no-service-required baseline backup. Put the result on another
# machine, a removable disk or a file share: never on the same volume
# as the data directory, or a dead volume takes the backup with it.
#
# Usage:
#   AGENTGATE_DATA=/var/lib/agentgate bash deploy/backup-offline.sh [DEST_DIR]
#
# (Run it with bash: the snapshot-push pipeline stores scripts without
# the executable bit, like deploy/install.sh — direct execution needs
# chmod +x first.)
#
# Output:
#   DEST_DIR/agentgate-offline-YYYYMMDD-HHMMSS/
#     state.json  audit.jsonl  audit-archive-*.jsonl.gz + .manifest.json
#     SHA256SUMS (one SHA-256 line per file)  RESTORE.txt (recovery steps)
#
# Run with the server stopped when you can (audit.jsonl is append-only;
# a copy taken mid-append can end in a torn last line — restore
# verification will flag it, but a clean copy is better). The script
# never deletes anything: pruning offline backups is irreversible and
# is a human decision.
set -euo pipefail

DATA_DIR="${AGENTGATE_DATA:-./data}"
DEST_DIR="${1:-./backups}"

if [ ! -d "$DATA_DIR" ]; then
  echo "backup-offline: data directory not found: $DATA_DIR" >&2
  exit 1
fi
if [ ! -f "$DATA_DIR/state.json" ] && [ ! -f "$DATA_DIR/audit.jsonl" ]; then
  echo "backup-offline: $DATA_DIR holds neither state.json nor audit.jsonl — nothing to back up" >&2
  exit 1
fi

STAMP="$(date -u +%Y%m%d-%H%M%S)"
OUT="$DEST_DIR/agentgate-offline-$STAMP"
mkdir -p "$OUT"

cp -a "$DATA_DIR/state.json" "$OUT/state.json" 2>/dev/null || echo "backup-offline: note: no state.json yet (fresh install)"
cp -a "$DATA_DIR/audit.jsonl" "$OUT/audit.jsonl" 2>/dev/null || echo "backup-offline: note: no audit.jsonl yet (no events recorded)"
cp -a "$DATA_DIR"/audit-archive-*.jsonl.gz "$OUT/" 2>/dev/null || true
cp -a "$DATA_DIR"/audit-archive-*.manifest.json "$OUT/" 2>/dev/null || true

cat > "$OUT/RESTORE.txt" <<'EOF'
AgentGate offline backup — recovery steps (see docs/backup.md §4):
  1. Stop the server (systemctl stop agentgate).
  2. Verify checksums: cd <this dir> && sha256sum -c SHA256SUMS
  3. Copy state.json, audit.jsonl and audit-archive-* into an empty
     data directory (or set AGENTGATE_DATA to this directory).
  4. Restore the SAME master key (AGENTGATE_MASTER_KEY) — without it
     the stored channel keys and tokens cannot be decrypted.
  5. Start the server and check GET /api/audit/verify: chain_valid
     must be true before serving traffic.
  6. If the chain does not verify, do NOT start the server on this
     copy — take the next backup (object storage or an older offline
     copy) and verify that one instead.
EOF

# Build the checksum manifest file-by-file (sha256sum aborts on a
# missing file, so a single glob call would drop the whole manifest
# on a fresh install without audit.jsonl).
: > "$OUT/SHA256SUMS"
for f in "$OUT"/state.json "$OUT"/audit.jsonl "$OUT"/audit-archive-*; do
  [ -f "$f" ] || continue
  (cd "$OUT" && sha256sum "$(basename "$f")") >> "$OUT/SHA256SUMS"
done

COUNT="$(find "$OUT" -type f | wc -l)"
echo "backup-offline: wrote $COUNT file(s) to $OUT"
echo "backup-offline: move this directory off the data volume (another machine, removable disk or file share)"
echo "backup-offline: verify with: cd $OUT && sha256sum -c SHA256SUMS"
