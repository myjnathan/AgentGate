#!/usr/bin/env python3
"""AgentGate demo end-to-end smoke test.

Expects a demo-mode server already running (see .github/workflows/ci.yml
or run locally: AGENTGATE_DEMO=1 AGENTGATE_MASTER_KEY=... ./agentgate).
Covers the blueprint acceptance path: login -> device-code onboarding ->
model gateway allow/block by tier -> MCP knowledge search -> audit chain.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("AGENTGATE_BASE", "http://127.0.0.1:8787")
failures = []


def check(name, cond, detail=""):
    print(("PASS " if cond else "FAIL ") + name + (f" :: {detail}" if detail and not cond else ""))
    if not cond:
        failures.append(name)


def call(path, data=None, token=None, method=None):
    req = urllib.request.Request(BASE + path, method=method or ("POST" if data is not None else "GET"))
    if data is not None:
        req.data = json.dumps(data).encode()
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    def parse(raw):
        try:
            return json.loads(raw or b"{}")
        except json.JSONDecodeError:
            return {"_text": raw.decode(errors="replace")}

    try:
        with urllib.request.urlopen(req) as r:
            return r.status, parse(r.read())
    except urllib.error.HTTPError as e:
        return e.code, parse(e.read())


def main():
    s, health = call("/healthz", method="GET")
    check("healthz", s == 200)

    s, login = call("/api/login", {"username": "lixiaofeng", "password": "demo12345"})
    check("employee login", s == 200 and "session" in login)
    sess = login["session"]

    s, start = call("/api/setup/device/start", {"agent_type": "claude-code", "tier": "byoa"})
    check("device start", s == 200 and start.get("code", "").startswith("AG-"))
    s, _ = call("/api/device/approve", {"code": start["code"], "device_name": "ci-smoke"}, token=sess)
    check("device approve", s == 200)
    s, poll = call("/api/setup/device/poll", {"code": start["code"], "secret": start["secret"]})
    tok = poll.get("token", "")
    check("device poll issues token once", s == 200 and tok.startswith("ag_at_"))
    s, poll2 = call("/api/setup/device/poll", {"code": start["code"], "secret": start["secret"]})
    check("second poll returns no token", not poll2.get("token"))

    s, models = call("/v1/models", token=tok, method="GET")
    check("models list", s == 200 and any(m["id"] == "mock-eco" for m in models.get("data", [])))

    s, chat = call("/v1/chat/completions",
                   {"model": "mock-eco", "messages": [{"role": "user", "content": "帮我写请假条"}]}, token=tok)
    check("clean chat allowed", s == 200 and "choices" in chat)

    s, chat = call("/v1/chat/completions",
                   {"model": "mock-pro", "messages": [{"role": "user", "content": "XH-200 的进度如何"}]}, token=tok)
    check("BYOA confidential blocked", s == 403 and chat.get("error", {}).get("code") == "data_level_forbidden")

    s, chat = call("/v1/chat/completions",
                   {"model": "mock-eco", "messages": [{"role": "user", "content": "包含 test_sensitive 内容"}]}, token=tok)
    check("banned word blocked", s == 400 and chat.get("error", {}).get("code") == "request_rejected")

    req = urllib.request.Request(BASE + "/mcp", data=json.dumps(
        {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
         "params": {"name": "kb_search", "arguments": {"query": "报销 住宿"}}}).encode())
    req.add_header("Authorization", "Bearer " + tok)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req) as r:
        mcp = json.loads(r.read())
    text = mcp.get("result", {}).get("content", [{}])[0].get("text", "")
    check("MCP kb_search finds policy doc", "kb_reimburse" in text)

    s, alogin = call("/api/login", {"username": "admin", "password": "agentgate-admin"})
    s, chans = call("/api/channels", token=alogin["session"], method="GET")
    check("channels list reports health", s == 200 and len(chans.get("channels", [])) >= 2
          and all(c.get("health") == "healthy" for c in chans["channels"]))
    s, ver = call("/api/audit/verify", token=alogin["session"], method="GET")
    check("audit chain valid", s == 200 and ver.get("chain_valid") is True)
    s, stats = call("/api/stats", token=alogin["session"], method="GET")
    check("stats reconcile with audit", s == 200
          and stats.get("totals", {}).get("calls", 0) == 3
          and stats.get("totals", {}).get("blocked", 0) == 2
          and stats.get("totals", {}).get("tokens", 0) > 0
          and any(b.get("key") == "mock-eco" for b in stats.get("by_model", []))
          and any(b.get("key") == "研发部" for b in stats.get("by_department", [])))
    s, stats = call("/api/stats", token=sess, method="GET")
    check("stats forbidden for employee", s == 403)

    # F3: per-token RPM. The smoke token already made 3 chat calls
    # above (allowed + 2 blocked — all counted at the gateway door),
    # so RPM=5 admits 2 more and rejects the 6th with 429.
    s, toks = call("/api/tokens", token=alogin["session"], method="GET")
    tok_id = next(t["id"] for t in toks.get("tokens", []) if t.get("agent_type") == "claude-code")
    s, upd = call("/api/tokens/" + tok_id, {"rpm_limit": 5}, token=alogin["session"], method="PATCH")
    check("token rpm update", s == 200 and upd.get("rpm_limit") == 5)
    codes, err_code = [], ""
    for _ in range(3):
        try:
            req = urllib.request.Request(BASE + "/v1/chat/completions",
                data=json.dumps({"model": "mock-eco", "messages": [{"role": "user", "content": "限流测试"}]}).encode())
            req.add_header("Authorization", "Bearer " + tok)
            req.add_header("Content-Type", "application/json")
            with urllib.request.urlopen(req) as r:
                codes.append(r.status)
        except urllib.error.HTTPError as e:
            codes.append(e.code)
            err_code = json.loads(e.read() or b"{}").get("error", {}).get("code", "")
    check("rpm limit enforced at 6th call", codes == [200, 200, 429] and err_code == "rate_limit_exceeded")

    print(f"\n{len(failures)} failure(s)")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
