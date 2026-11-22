#!/usr/bin/env python3
"""Assemble the user package (backlog F20) from `make build` output.

The user package is the employee installable: ag-guard + ag-setup plus
a generated guard.json (gateway URL, data level, egress allowlist) and
a MANIFEST.json with the SHA-256 of every file. It is the offline twin
of POST /api/packages/user-bundle: same zip layout, same guard.json
shape, so an admin can generate it in CI, upload it to the platform
(which re-verifies the hash), and employees download the same artifact.

Environment:
    AGENTGATE_USER_NAME        package name (default agentgate-user)
    AGENTGATE_USER_VERSION     package version (default: git describe or dev)
    AGENTGATE_USER_AGENT_TYPE  agent type the bundle installs for (default dsh)
    AGENTGATE_USER_GATEWAY_URL gateway base URL (default http://127.0.0.1:8787)
    AGENTGATE_USER_MAX_LEVEL   highest data level (default internal)
    AGENTGATE_USER_EGRESS      egress allowlist, comma-separated hostnames
    AGENTGATE_USER_DRY_RUN     "1" writes dry_run=true into guard.json

Output: dist/agentgate-user-<version>.zip (+ a staging dir
dist/user-bundle/). Prints the zip's SHA-256: register it with
POST /api/packages/{id}/baseline after uploading, or let the
platform register it at generation time.
"""

import hashlib
import json
import os
import shutil
import subprocess
import sys
import zipfile
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    name = os.environ.get("AGENTGATE_USER_NAME", "agentgate-user")
    version = os.environ.get("AGENTGATE_USER_VERSION", "")
    if not version:
        try:
            version = subprocess.check_output(
                ["git", "describe", "--tags", "--always", "--dirty"],
                cwd=ROOT, text=True).strip()
        except Exception:
            version = "dev"
    agent_type = os.environ.get("AGENTGATE_USER_AGENT_TYPE", "dsh")
    gateway = os.environ.get("AGENTGATE_USER_GATEWAY_URL", "http://127.0.0.1:8787")
    max_level = os.environ.get("AGENTGATE_USER_MAX_LEVEL", "internal")
    egress = [e.strip() for e in os.environ.get("AGENTGATE_USER_EGRESS", "").split(",") if e.strip()]
    dry_run = os.environ.get("AGENTGATE_USER_DRY_RUN", "") == "1"

    if max_level not in ("public", "internal", "confidential", "secret"):
        print(f"bad AGENTGATE_USER_MAX_LEVEL: {max_level}", file=sys.stderr)
        return 1
    if not (gateway.startswith("https://") or gateway.startswith("http://127.0.0.1")
            or gateway.startswith("http://localhost")):
        print(f"bad AGENTGATE_USER_GATEWAY_URL: {gateway}", file=sys.stderr)
        return 1

    for binary in ("ag-guard", "ag-setup"):
        if not os.path.isfile(os.path.join(DIST, binary)):
            print(f"missing dist/{binary}: run `make build` first", file=sys.stderr)
            return 1

    staging = os.path.join(DIST, "user-bundle")
    shutil.rmtree(staging, ignore_errors=True)
    os.makedirs(staging)
    for binary in ("ag-guard", "ag-setup"):
        shutil.copy2(os.path.join(DIST, binary), os.path.join(staging, binary))

    # guard.json: the sidecar reads gateway_url (extra fields are
    # ignored by ag-guard); the policy fields document this install.
    generated_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    guard = {
        "gateway_url": gateway,
        "max_data_level": max_level,
        "egress_allowlist": egress,
        "agent_type": agent_type,
        "package": name,
        "version": version,
    }
    if dry_run:
        guard["dry_run"] = True
    guard_bytes = (json.dumps(guard, indent=2, ensure_ascii=False) + "\n").encode()
    with open(os.path.join(staging, "guard.json"), "wb") as f:
        f.write(guard_bytes)

    components = []
    for role in ("ag-guard", "ag-setup"):
        path = os.path.join(staging, role)
        components.append({
            "role": role,
            "filename": role,
            "sha256": sha256_file(path),
            "size_bytes": os.path.getsize(path),
        })
    manifest = {
        "name": name,
        "version": version,
        "agent_type": agent_type,
        "generated_at": generated_at,
        "gateway_url": gateway,
        "components": components,
        "guard_config_sha256": hashlib.sha256(guard_bytes).hexdigest(),
    }
    with open(os.path.join(staging, "MANIFEST.json"), "w") as f:
        json.dump(manifest, f, indent=2)
        f.write("\n")

    zip_path = os.path.join(DIST, f"{name}-{version}.zip")
    with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as zf:
        for entry in ("ag-guard", "ag-setup", "guard.json", "MANIFEST.json"):
            zf.write(os.path.join(staging, entry), entry)
    print(f"user bundle: {zip_path}")
    print(f"sha256: {sha256_file(zip_path)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
