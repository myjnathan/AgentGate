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


def call(path, data=None, token=None, method=None, headers=None):
    req = urllib.request.Request(BASE + path, method=method or ("POST" if data is not None else "GET"))
    if data is not None:
        req.data = json.dumps(data).encode()
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if headers:
        for k, v in headers.items():
            req.add_header(k, v)
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

    # F4: audit export (NDJSON + chain verification headers) and a
    # no-op archive. Export/archive events are Kind "admin", so they
    # do not disturb the stats totals checked above.
    admin_tok = alogin.get("session")
    req = urllib.request.Request(BASE + "/api/audit/export")
    req.add_header("Authorization", "Bearer " + admin_tok)
    with urllib.request.urlopen(req) as r:
        raw = r.read().decode()
        lines = [ln for ln in raw.splitlines() if ln.strip()]
        count = int(r.headers.get("X-Audit-Event-Count", "0"))
        check("audit export NDJSON + chain headers", r.status == 200
              and r.headers.get("Content-Type", "").startswith("application/x-ndjson")
              and "attachment" in r.headers.get("Content-Disposition", "")
              and r.headers.get("X-Audit-Chain-Valid") == "true"
              and count == len(lines) and count > 0
              and all(json.loads(ln).get("seq") for ln in lines))
    s, arch = call("/api/audit/archive", {"before": "2020-01-01T00:00:00Z"}, token=admin_tok, method="POST")
    check("audit archive no-op for old cutoff", s == 200 and arch.get("archived") == 0
          and arch.get("chain_valid") is True)
    s, ver = call("/api/audit/verify", token=admin_tok, method="GET")
    check("audit chain still valid after export/archive", s == 200 and ver.get("chain_valid") is True)

    # F5: webhook alerts. A local receiver stands in for the enterprise
    # chat robot; a banned-word block must deliver a structured alert
    # with no sensitive text. A second (managed) token is onboarded so
    # the extra call does not disturb the RPM accounting below.
    import threading
    import time as _time
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    received = []

    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("Content-Length", "0"))
            received.append(self.rfile.read(n))
            self.send_response(200)
            self.end_headers()
        def log_message(self, *a):
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    hook_url = f"http://127.0.0.1:{httpd.server_address[1]}/hook"
    s, wh = call("/api/alerts/webhooks",
                 {"name": "smoke", "url": hook_url, "kind": "generic"}, token=admin_tok)
    check("webhook create", s == 201 and wh.get("id", "").startswith("wh_"))
    s, _ = call("/api/alerts/webhooks",
                {"name": "x", "url": hook_url, "kind": "generic"}, token=sess)
    check("webhook create forbidden for employee", s == 403)
    s, start2 = call("/api/setup/device/start", {"agent_type": "dsh", "tier": "managed"})
    s, _ = call("/api/device/approve", {"code": start2["code"], "device_name": "ci-smoke-alert"}, token=admin_tok)
    s, poll2 = call("/api/setup/device/poll", {"code": start2["code"], "secret": start2["secret"]})
    tok2 = poll2.get("token", "")
    s, chat = call("/v1/chat/completions",
                   {"model": "mock-eco", "messages": [{"role": "user", "content": "包含 test_sensitive 内容"}]}, token=tok2)
    check("alert token banned word blocked", s == 400)
    for _ in range(100):
        if received:
            break
        _time.sleep(0.1)
    alert_ok = False
    if received:
        try:
            payload = json.loads(received[0])
            alert_ok = (payload.get("source") == "agentgate"
                        and payload.get("severity") == "critical"
                        and payload.get("action") == "model_call_blocked"
                        and payload.get("actor") == "admin"
                        and "test_sensitive" not in received[0].decode(errors="replace"))
        except json.JSONDecodeError:
            alert_ok = False
    check("webhook receives structured alert without sensitive text", alert_ok)
    s, tst = call("/api/alerts/webhooks/" + wh.get("id", "") + "/test", {}, token=admin_tok, method="POST")
    check("webhook test endpoint delivers", s == 200 and tst.get("delivered") is True)
    httpd.shutdown()

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

    # F6: MCP federation. A local fake upstream MCP server stands in
    # for an enterprise system (e.g. HR); its tools must appear
    # namespaced, forward calls, and respect tier and data-level gates.
    class FakeUpstream(BaseHTTPRequestHandler):
        def do_POST(self):
            n = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(n) or b"{}")
            method = req.get("method")
            if method == "tools/list":
                result = {"tools": [
                    {"name": "leave_balance", "description": "remaining leave days",
                     "inputSchema": {"type": "object", "properties": {}}},
                    {"name": "project_status", "description": "project status",
                     "inputSchema": {"type": "object", "properties": {}}}]}
            elif method == "tools/call":
                name = req.get("params", {}).get("name")
                text = {"leave_balance": '{"balance": 5, "employee": "lixiaofeng"}',
                        "project_status": "XH-200 进度正常"}.get(name, "ok")
                result = {"content": [{"type": "text", "text": text}], "isError": False}
            else:
                result = {}
            body = json.dumps({"jsonrpc": "2.0", "id": req.get("id", 1), "result": result}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        def log_message(self, *a):
            pass

    up_httpd = ThreadingHTTPServer(("127.0.0.1", 0), FakeUpstream)
    threading.Thread(target=up_httpd.serve_forever, daemon=True).start()
    up_url = f"http://127.0.0.1:{up_httpd.server_address[1]}/mcp"

    def mcp_rpc(token, method, params=None):
        req = urllib.request.Request(BASE + "/mcp", data=json.dumps(
            {"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}).encode())
        req.add_header("Authorization", "Bearer " + token)
        req.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(req) as r:
            return json.loads(r.read())

    s, up = call("/api/mcp/upstreams",
                 {"name": "hr", "url": up_url, "max_data_level": "internal", "min_tier": "byoa"},
                 token=admin_tok)
    check("upstream hr registered", s == 201 and up.get("id", "").startswith("up_"))
    s, up2 = call("/api/mcp/upstreams",
                  {"name": "ops", "url": up_url, "max_data_level": "confidential", "min_tier": "managed"},
                  token=admin_tok)
    check("upstream ops registered", s == 201 and up2.get("id", "").startswith("up_"))
    s, _ = call("/api/mcp/upstreams",
                {"name": "hr", "url": up_url}, token=admin_tok)
    check("duplicate upstream namespace rejected", s == 409)
    s, _ = call("/api/mcp/upstreams",
                {"name": "hr2", "url": up_url}, token=sess)
    check("upstream create forbidden for employee", s == 403)
    s, tst = call("/api/mcp/upstreams/" + up.get("id", "") + "/test", {}, token=admin_tok, method="POST")
    check("upstream test lists namespaced tools", s == 200 and tst.get("ok") is True
          and "hr.leave_balance" in tst.get("tools", []))

    listed = mcp_rpc(tok, "tools/list")
    names = [t["name"] for t in listed.get("result", {}).get("tools", [])]
    check("federated tools listed namespaced for BYOA",
          "hr.leave_balance" in names and "ops.leave_balance" not in names)
    listed2 = mcp_rpc(tok2, "tools/list")
    names2 = [t["name"] for t in listed2.get("result", {}).get("tools", [])]
    check("managed token sees managed-only upstream tools", "ops.leave_balance" in names2)

    res = mcp_rpc(tok, "tools/call", {"name": "hr.leave_balance", "arguments": {}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("federated tool call forwards to upstream", '"balance": 5' in text)
    res = mcp_rpc(tok, "tools/call", {"name": "ops.leave_balance", "arguments": {}})
    check("BYOA call to managed-only upstream denied",
          res.get("error", {}).get("code") == -32003)
    res = mcp_rpc(tok, "tools/call", {"name": "hr.leave_balance", "arguments": {"note": "XH-200"}})
    check("confidential args blocked from internal-only upstream",
          res.get("error", {}).get("code") == -32004)
    res = mcp_rpc(tok, "tools/call", {"name": "hr.project_status", "arguments": {}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("federated result redacted for BYOA",
          "XH-200" not in text and "<CUSTOM_1>" in text)
    res = mcp_rpc(tok2, "tools/call", {"name": "hr.project_status", "arguments": {}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("managed token receives raw federated result", "XH-200" in text)
    up_httpd.shutdown()

    # F7: REST -> MCP adapter. The demo seed already registers
    # rest.leave_balance against the mock HR endpoint; a local fake
    # REST server stands in for a second enterprise system to cover
    # POST bodies, schema validation and tier gating.
    res = mcp_rpc(tok, "tools/call", {"name": "rest.leave_balance", "arguments": {"employee_id": "lixiaofeng"}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("seeded REST tool calls demo HR endpoint", '"balance_days":12' in text)
    res = mcp_rpc(tok, "tools/call", {"name": "rest.leave_balance", "arguments": {}})
    check("REST tool schema validation rejects missing arg", res.get("error", {}).get("code") == -32602)

    class FakeREST(BaseHTTPRequestHandler):
        def do_GET(self):
            from urllib.parse import urlparse, parse_qs
            u = urlparse(self.path)
            if u.path == "/hr/leave-balance":
                emp = parse_qs(u.query).get("employee_id", [""])[0]
                body = json.dumps({"employee_id": emp, "balance_days": 12}).encode()
            elif u.path == "/hr/notice":
                body = "XH-200 进度正常".encode()
            else:
                self.send_response(404); self.end_headers(); return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body)
        def do_POST(self):
            n = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(n) or b"{}")
            body = json.dumps({"status": "submitted", "employee_id": req.get("employee_id"),
                               "days": req.get("days")}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers(); self.wfile.write(body)
        def log_message(self, *a):
            pass

    rest_httpd = ThreadingHTTPServer(("127.0.0.1", 0), FakeREST)
    threading.Thread(target=rest_httpd.serve_forever, daemon=True).start()
    rest_url = f"http://127.0.0.1:{rest_httpd.server_address[1]}"

    s, rt = call("/api/mcp/rest-tools",
                 {"name": "fake_balance", "base_url": rest_url, "method": "GET", "path": "/hr/leave-balance",
                  "input_schema": {"type": "object", "properties": {"employee_id": {"type": "string"}},
                                   "required": ["employee_id"]},
                  "max_data_level": "internal", "min_tier": "byoa"},
                 token=admin_tok)
    check("REST tool registered", s == 201 and rt.get("id", "").startswith("rt_"))
    s, rt2 = call("/api/mcp/rest-tools",
                  {"name": "fake_request", "base_url": rest_url, "method": "POST", "path": "/hr/leave-request",
                   "input_schema": {"type": "object", "properties": {"employee_id": {"type": "string"}, "days": {"type": "integer"}},
                                    "required": ["employee_id", "days"]},
                   "max_data_level": "internal", "min_tier": "managed"},
                  token=admin_tok)
    check("managed-only REST tool registered", s == 201)
    s, rt3 = call("/api/mcp/rest-tools",
                  {"name": "fake_notice", "base_url": rest_url, "method": "GET", "path": "/hr/notice",
                   "input_schema": {"type": "object", "properties": {}},
                   "max_data_level": "internal", "min_tier": "byoa"},
                  token=admin_tok)
    check("notice REST tool registered", s == 201)
    s, _ = call("/api/mcp/rest-tools",
                {"name": "fake_balance", "base_url": rest_url, "method": "GET", "path": "/hr/leave-balance",
                 "input_schema": {"type": "object", "properties": {}}},
                token=admin_tok)
    check("duplicate REST tool name rejected", s == 409)
    s, _ = call("/api/mcp/rest-tools",
                {"name": "bad_tool", "base_url": rest_url, "method": "GET", "path": "/hr/leave-balance",
                 "input_schema": {"type": "object", "properties": {}, "required": ["nope"]}},
                token=admin_tok)
    check("invalid input schema rejected", s == 400)
    s, _ = call("/api/mcp/rest-tools",
                {"name": "emp_tool", "base_url": rest_url, "method": "GET", "path": "/x",
                 "input_schema": {"type": "object", "properties": {}}},
                token=sess)
    check("REST tool create forbidden for employee", s == 403)
    s, tst = call("/api/mcp/rest-tools/" + rt.get("id", "") + "/test",
                  {"arguments": {"employee_id": "lixiaofeng"}}, token=admin_tok, method="POST")
    check("REST tool test calls upstream", s == 200 and tst.get("ok") is True
          and "balance_days" in tst.get("preview", ""))

    listed = mcp_rpc(tok, "tools/list")
    names = [t["name"] for t in listed.get("result", {}).get("tools", [])]
    check("REST tools listed namespaced for BYOA",
          "rest.fake_balance" in names and "rest.fake_request" not in names)
    res = mcp_rpc(tok, "tools/call", {"name": "rest.fake_balance", "arguments": {"employee_id": "lixiaofeng"}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("REST GET tool forwards query param", '"balance_days": 12' in text)
    res = mcp_rpc(tok2, "tools/call", {"name": "rest.fake_request", "arguments": {"employee_id": "chenjing", "days": 2}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("REST POST tool forwards JSON body", '"status": "submitted"' in text and '"days": 2' in text)
    res = mcp_rpc(tok, "tools/call", {"name": "rest.fake_request", "arguments": {"employee_id": "li", "days": 1}})
    check("BYOA call to managed-only REST tool denied", res.get("error", {}).get("code") == -32003)
    res = mcp_rpc(tok, "tools/call", {"name": "rest.fake_balance", "arguments": {"employee_id": "XH-200"}})
    check("confidential args blocked from internal-only REST tool", res.get("error", {}).get("code") == -32004)
    res = mcp_rpc(tok, "tools/call", {"name": "rest.fake_notice", "arguments": {}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("REST result redacted for BYOA", "XH-200" not in text and "<CUSTOM_1>" in text)
    res = mcp_rpc(tok2, "tools/call", {"name": "rest.fake_notice", "arguments": {}})
    text = res.get("result", {}).get("content", [{}])[0].get("text", "")
    check("managed token receives raw REST result", "XH-200" in text)
    rest_httpd.shutdown()

    # F17: channel trust types and the tiered routing gradient.
    s, chans = call("/api/channels", token=admin_tok, method="GET")
    trust_by_name = {c.get("name"): c.get("trust_type") for c in chans.get("channels", [])}
    check("seeded channels carry trust types",
          trust_by_name.get("cloud-contract (mock)") == "signed_cloud"
          and trust_by_name.get("cloud-contract-desensitized (mock)") == "signed_cloud"
          and trust_by_name.get("private-onprem (mock)") == "dedicated_cloud"
          and trust_by_name.get("local-model (mock)") == "local_model")
    # Confidential data takes the default route: desensitized signed
    # cloud, with the original value restored in the response.
    s, chat = call("/v1/chat/completions",
                   {"model": "mock-pro", "messages": [{"role": "user", "content": "请分析 XH-200 项目的进展"}]}, token=tok2)
    text = json.dumps(chat, ensure_ascii=False)
    check("managed confidential defaults to desensitized cloud", s == 200 and "XH-200" in text)
    # Top-secret data (header-declared) only reaches the local model.
    s, chat = call("/v1/chat/completions",
                   {"model": "mock-pro", "messages": [{"role": "user", "content": "普通内容"}]}, token=tok2,
                   headers={"X-AgentGate-Data-Level": "secret"})
    check("managed secret routes to local model", s == 200 and "choices" in chat)
    # A model served only by a dedicated-cloud channel has no compliant
    # channel for secret data: explicit block, not a generic error.
    s, vpc_ch = call("/api/channels",
                     {"name": "vpc-only (smoke)", "base_url": BASE + "/mock/v1", "api_key": "smoke-key",
                      "models": ["mock-vpc-only"], "max_data_level": "secret",
                      "trust_type": "dedicated_cloud"}, token=admin_tok)
    check("dedicated-cloud channel created with trust type", s == 201 and vpc_ch.get("id", "").startswith("ch_"))
    s, chat = call("/v1/chat/completions",
                   {"model": "mock-vpc-only", "messages": [{"role": "user", "content": "普通内容"}]}, token=tok2,
                   headers={"X-AgentGate-Data-Level": "secret"})
    check("secret blocked with no compliant channel",
          s == 403 and chat.get("error", {}).get("code") == "no_compliant_channel")
    s, _ = call("/api/channels",
                {"name": "bad-trust", "base_url": BASE + "/mock/v1",
                 "models": ["mock-eco"], "trust_type": "onprem"}, token=admin_tok)
    check("invalid trust type rejected", s == 400)
    s, _ = call("/api/channels",
                {"name": "emp-channel", "base_url": BASE + "/mock/v1",
                 "models": ["mock-eco"]}, token=sess)
    check("channel create forbidden for employee", s == 403)

    # F12: channel multi-key rotation — add / disable / re-enable /
    # delete keys; only masked forms are ever returned.
    s, mk = call("/api/channels",
                 {"name": "multi-key (smoke)", "base_url": BASE + "/mock/v1",
                  "api_key": "smoke-key-one", "api_keys": ["smoke-key-two", "smoke-key-three"],
                  "models": ["mock-eco"], "max_data_level": "internal"}, token=admin_tok)
    check("multi-key channel created with 3 keys", s == 201 and mk.get("key_count") == 3)
    mk_id = mk.get("id", "")
    s, chans = call("/api/channels", token=admin_tok, method="GET")
    mk_ch = [c for c in chans.get("channels", []) if c.get("id") == mk_id]
    check("channel lists 3 masked keys, none disabled",
          s == 200 and len(mk_ch) == 1 and len(mk_ch[0].get("keys", [])) == 3
          and all(k.get("masked", "").startswith("****") and not k.get("disabled") for k in mk_ch[0]["keys"])
          and "smoke-key-one" not in json.dumps(chans))
    s, added = call("/api/channels/" + mk_id + "/keys", {"api_key": "smoke-key-four"}, token=admin_tok)
    check("fourth key added", s == 201 and added.get("index") == 3 and added.get("key_masked") == "****four")
    s, _ = call("/api/channels/" + mk_id + "/keys", {"api_key": "smoke-key-two"}, token=admin_tok)
    check("duplicate key rejected", s == 400)
    s, _ = call("/api/channels/" + mk_id + "/keys/3/disable", {"reason": "smoke test"}, token=admin_tok)
    s, chans = call("/api/channels", token=admin_tok, method="GET")
    mk_ch = [c for c in chans.get("channels", []) if c.get("id") == mk_id]
    check("key disabled with reason",
          len(mk_ch) == 1 and mk_ch[0]["keys"][3].get("disabled") is True
          and mk_ch[0]["keys"][3].get("disabled_reason") == "smoke test")
    s, _ = call("/api/channels/" + mk_id + "/keys/3/enable", {}, token=admin_tok)
    s, chans = call("/api/channels", token=admin_tok, method="GET")
    mk_ch = [c for c in chans.get("channels", []) if c.get("id") == mk_id]
    check("key re-enabled",
          len(mk_ch) == 1 and mk_ch[0]["keys"][3].get("disabled") is False)
    s, _ = call("/api/channels/" + mk_id + "/keys/3", token=admin_tok, method="DELETE")
    s, chans = call("/api/channels", token=admin_tok, method="GET")
    mk_ch = [c for c in chans.get("channels", []) if c.get("id") == mk_id]
    check("key deleted, 3 keys remain",
          len(mk_ch) == 1 and len(mk_ch[0].get("keys", [])) == 3)
    s, _ = call("/api/channels/" + mk_id + "/keys", {"api_key": "smoke-key-x"}, token=sess)
    check("employee cannot add channel key", s == 403)
    s, single = call("/api/channels",
                     {"name": "single-key (smoke)", "base_url": BASE + "/mock/v1",
                      "api_key": "only-smoke-key", "models": ["mock-eco"]}, token=admin_tok)
    s, _ = call("/api/channels/" + single.get("id", "") + "/keys/0", token=admin_tok, method="DELETE")
    check("last key cannot be deleted", s == 400)

    # F8: prompt templates — versioned, reviewed, published; agents
    # only ever receive the published version via prompts/get.
    s, plist = call("/api/prompts", token=admin_tok, method="GET")
    seed = [p for p in plist.get("prompts", []) if p.get("name") == "weekly_report"]
    check("seeded weekly_report template listed",
          s == 200 and len(seed) == 1 and seed[0].get("status") == "published"
          and seed[0].get("published_version") == 1)
    listed = mcp_rpc(tok, "prompts/list")
    names = [p["name"] for p in listed.get("result", {}).get("prompts", [])]
    check("seeded template listed for BYOA via prompts/list", "weekly_report" in names)
    res = mcp_rpc(tok, "prompts/get", {"name": "weekly_report",
                                       "arguments": {"name": "李晓峰", "department": "研发部", "week": "2026-W47"}})
    text = res.get("result", {}).get("messages", [{}])[0].get("content", {}).get("text", "")
    check("prompts/get renders published template",
          "研发部的李晓峰" in text and "2026-W47" in text and "{{" not in text)
    res = mcp_rpc(tok, "prompts/get", {"name": "weekly_report", "arguments": {"name": "李晓峰"}})
    check("prompts/get rejects missing required argument", res.get("error", {}).get("code") == -32602)
    # Employee submits a template; admin reviews it; a second version
    # does not publish itself; explicit publish and rollback move the
    # served version.
    s, pt = call("/api/prompts",
                 {"name": "smoke_greet", "description": "冒烟问候模板", "scope": "*",
                  "content": "你好，{{name}}。",
                  "arguments": [{"name": "name", "description": "姓名", "required": True}],
                  "max_data_level": "internal", "min_tier": "byoa"},
                 token=sess)
    check("employee submits prompt template", s == 201 and pt.get("status") == "in_review")
    pt_id = pt.get("id", "")
    s, _ = call("/api/prompts", {"name": "smoke_greet", "content": "x"}, token=admin_tok)
    check("duplicate prompt template name rejected", s == 409)
    s, _ = call("/api/prompts", {"name": "BadName", "content": "x {{name}}",
                                 "arguments": [{"name": "name", "required": True}]}, token=admin_tok)
    check("invalid prompt template name rejected", s == 400)
    s, ver = call("/api/prompts/" + pt_id + "/versions",
                  {"content": "你好呀，{{name}}。",
                   "arguments": [{"name": "name", "description": "姓名", "required": True}]},
                  token=sess, method="POST")
    check("new prompt version created", s == 201 and ver.get("version") == 2)
    res = mcp_rpc(tok, "prompts/get", {"name": "smoke_greet", "arguments": {"name": "李晓峰"}})
    check("unpublished template hidden from agents", res.get("error", {}).get("code") == -32602)
    s, rev = call("/api/prompts/" + pt_id + "/review", {"approve": True}, token=admin_tok, method="POST")
    check("review approve publishes current version",
          s == 200 and rev.get("status") == "published" and rev.get("published_version") == 2)
    res = mcp_rpc(tok, "prompts/get", {"name": "smoke_greet", "arguments": {"name": "李晓峰"}, "version": "2"})
    text = res.get("result", {}).get("messages", [{}])[0].get("content", {}).get("text", "")
    check("published v2 served after review", "你好呀，李晓峰" in text)
    res = mcp_rpc(tok, "prompts/get", {"name": "smoke_greet", "arguments": {"name": "李晓峰"}, "version": "1"})
    check("unpublished version not served by pin", res.get("error", {}).get("code") == -32602)
    s, pub = call("/api/prompts/" + pt_id + "/publish", {"version": 1}, token=admin_tok, method="POST")
    check("rollback publishes v1", s == 200 and pub.get("published_version") == 1)
    res = mcp_rpc(tok, "prompts/get", {"name": "smoke_greet", "arguments": {"name": "李晓峰"}})
    text = res.get("result", {}).get("messages", [{}])[0].get("content", {}).get("text", "")
    check("rollback serves v1 again", "你好，李晓峰" in text and "你好呀" not in text)
    res = mcp_rpc(tok, "prompts/get", {"name": "smoke_greet",
                                       "arguments": {"name": "XH-200"}})
    check("confidential argument blocked from internal template", res.get("error", {}).get("code") == -32004)
    s, _ = call("/api/prompts/" + pt_id, token=admin_tok, method="DELETE")
    check("prompt template deleted", s == 200)

    # F9: OIDC login (demo smoke runs without an IdP configured, so the
    # disabled paths are asserted here; the enabled loop is covered by
    # Go tests against a fake IdP and a live run against the Duende demo).
    s, prov = call("/api/auth/providers", method="GET")
    check("auth providers reports local login", s == 200 and prov.get("local_enabled") is True)
    check("auth providers reports oidc disabled in demo", s == 200 and prov.get("oidc_enabled") is False)
    s, _ = call("/auth/oidc/login", method="GET")
    check("oidc login without IdP is unavailable", s == 503)
    s, _ = call("/auth/oidc/callback?code=x&state=y", method="GET")
    check("oidc callback without IdP is unavailable", s == 503)
    s, _ = call("/api/auth/oidc/status", token=sess, method="GET")
    check("oidc status requires admin", s == 403)

    # F10: streaming desensitization restore + tool_calls protection
    # (blueprint probe 3). A fresh BYOA token keeps the RPM accounting
    # of the earlier tokens untouched.
    s, start3 = call("/api/setup/device/start", {"agent_type": "dsh", "tier": "byoa"})
    s, _ = call("/api/device/approve", {"code": start3["code"], "device_name": "ci-smoke-f10"}, token=sess)
    s, poll3 = call("/api/setup/device/poll", {"code": start3["code"], "secret": start3["secret"]})
    tok3 = poll3.get("token", "")
    # Streaming call with a phone number: the managed token's reply
    # must carry the original value, never a placeholder, and end
    # with data: [DONE].
    req = urllib.request.Request(BASE + "/v1/chat/completions",
        data=json.dumps({"model": "mock-eco", "stream": True,
                         "messages": [{"role": "user", "content": "我的手机号是13812345678，帮我记下来"}]}).encode())
    req.add_header("Authorization", "Bearer " + tok2)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req) as r:
        sse = r.read().decode(errors="replace")
    check("streaming redacted placeholder restored", "13812345678" in sse
          and "PII_1" not in sse and sse.strip().endswith("data: [DONE]"))
    # Banned word inside tool_calls arguments is blocked like content.
    s, chat = call("/v1/chat/completions",
                   {"model": "mock-eco", "messages": [
                       {"role": "user", "content": "帮我发个短信"},
                       {"role": "assistant", "tool_calls": [
                           {"id": "call_1", "type": "function", "function": {
                               "name": "send_sms",
                               "arguments": '{"text":"this contains test_sensitive word"}'}}]}]},
                   token=tok2)
    check("banned word in tool_calls blocked", s == 400 and chat.get("error", {}).get("code") == "request_rejected")
    # Confidential tool_calls arguments classify the request: BYOA refused.
    s, chat = call("/v1/chat/completions",
                   {"model": "mock-pro", "messages": [
                       {"role": "user", "content": "帮我查项目"},
                       {"role": "assistant", "tool_calls": [
                           {"id": "call_1", "type": "function", "function": {
                               "name": "project_status",
                               "arguments": '{"project":"XH-200"}'}}]}]},
                   token=tok3)
    check("confidential tool_calls blocked for BYOA", s == 403 and chat.get("error", {}).get("code") == "data_level_forbidden")

    # F18: folder knowledge base — demo folders are seeded in demo
    # mode (internal 公司制度 with a .md file, confidential 机密资料
    # with a .pdf). Progressive disclosure: catalog -> structure ->
    # section, with the confidential folder invisible to BYOA.
    s, folders = call("/api/kb/folders", token=admin_tok, method="GET")
    fl = folders.get("folders", [])
    check("folder sources listed in demo", s == 200 and len(fl) == 2
          and any(f.get("name") == "公司制度" and f.get("docs", 0) == 1 for f in fl)
          and any(f.get("name") == "机密资料" and f.get("docs", 0) == 1 for f in fl))
    s, _ = call("/api/kb/folders", token=sess, method="GET")
    check("folder list forbidden for employee", s == 403)

    def mcp_text(token, tool, args):
        r = mcp_rpc(token, "tools/call", {"name": tool, "arguments": args})
        return (r.get("result", {}).get("content", [{}])[0].get("text", ""),
                bool(r.get("result", {}).get("isError")))

    text, _ = mcp_text(tok, "kb_search", {"query": "请假 年假"})
    check("MCP kb_search finds folder md doc", "考勤与请假制度" in text and '"source":"folder"' in text)
    text, _ = mcp_text(tok, "kb_search", {"query": "供应商 准入"})
    check("MCP kb_search hides confidential folder from BYOA", "供应商准入要求" not in text)
    text, _ = mcp_text(tok2, "kb_search", {"query": "供应商 准入"})
    check("MCP kb_search finds confidential folder for managed", "供应商准入要求" in text)

    cat_text, _ = mcp_text(tok, "kb_catalog", {})
    cat = json.loads(cat_text)
    docs = cat.get("documents", [])
    check("MCP kb_catalog lists internal folder doc", any(d.get("title") == "考勤与请假制度" for d in docs)
          and not any(d.get("title") == "供应商准入要求" for d in docs))
    doc_id = next(d["doc_id"] for d in docs if d.get("title") == "考勤与请假制度")
    st_text, _ = mcp_text(tok, "kb_structure", {"doc_id": doc_id})
    stj = json.loads(st_text)
    node_id = stj["tree"]["children"][0]["node_id"]
    sec_text, is_err = mcp_text(tok, "kb_section", {"doc_id": doc_id, "node_id": node_id})
    sec = json.loads(sec_text)
    check("MCP kb_section returns cited text", not is_err and sec.get("section") == "请假流程"
          and "年假每年 5 天起" in sec.get("text", "") and sec.get("file", "").endswith(".md")
          and sec.get("pages", "") != "")
    # Rescan of the demo folder is a no-op (nothing changed).
    fid = next(f["id"] for f in fl if f.get("name") == "公司制度")
    s, rs = call("/api/kb/folders/" + fid + "/rescan", {}, token=admin_tok, method="POST")
    check("folder rescan no-op when unchanged", s == 200 and rs.get("stats", {}).get("unchanged") == 1
          and rs.get("stats", {}).get("added") == 0)

    # F11: exact-match response cache. Only clean public/internal
    # non-streaming calls are cached; a hit serves the stored body
    # (the same `created` second proves it — hence the sleep) and
    # does not count toward upstream usage. Go canonicalizes the
    # header name to X-Agentgate-Cache on the wire.
    def chat_raw(token, payload, headers=None):
        req = urllib.request.Request(BASE + "/v1/chat/completions",
            data=json.dumps(payload).encode())
        req.add_header("Authorization", "Bearer " + token)
        req.add_header("Content-Type", "application/json")
        if headers:
            for k, v in headers.items():
                req.add_header(k, v)
        try:
            with urllib.request.urlopen(req) as r:
                return r.status, dict(r.headers), json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), json.loads(e.read() or b"{}")

    s, cache0 = call("/api/cache", token=admin_tok, method="GET")
    check("cache status reports enabled", s == 200 and cache0.get("enabled") is True
          and cache0.get("ttl_seconds", 0) >= 60 and "hits" in cache0 and "misses" in cache0)
    s, _ = call("/api/cache", token=sess, method="GET")
    check("cache status forbidden for employee", s == 403)

    cache_q = {"model": "mock-eco", "messages": [{"role": "user", "content": "缓存冒烟：星河精密的考勤制度是什么"}]}
    s1, h1, b1 = chat_raw(tok3, cache_q)
    check("first identical call is a cache MISS", s1 == 200 and h1.get("X-Agentgate-Cache") == "MISS" and "choices" in b1)
    _time.sleep(1.2)
    s2, h2, b2 = chat_raw(tok3, cache_q)
    check("second identical call is a cache HIT with identical body",
          s2 == 200 and h2.get("X-Agentgate-Cache") == "HIT" and b2 == b1)
    s3, h3, _ = chat_raw(tok3, {**cache_q, "temperature": 0.7})
    check("same prompt with different temperature is a MISS", s3 == 200 and h3.get("X-Agentgate-Cache") == "MISS")

    pii_q = {"model": "mock-eco", "messages": [{"role": "user", "content": "我的手机号是13812345678，帮我记下来"}]}
    _, hp1, _ = chat_raw(tok3, pii_q)
    _time.sleep(1.2)
    _, hp2, _ = chat_raw(tok3, pii_q)
    check("request with PII never consults the cache",
          hp1.get("X-Agentgate-Cache") is None and hp2.get("X-Agentgate-Cache") is None)

    s, tog = call("/api/cache", {"enabled": False}, token=admin_tok, method="POST")
    check("cache toggle off", s == 200 and tog.get("enabled") is False)
    _, hd1, _ = chat_raw(tok3, cache_q)
    _, hd2, _ = chat_raw(tok3, cache_q)
    check("disabled cache serves no hits",
          hd1.get("X-Agentgate-Cache") is None and hd2.get("X-Agentgate-Cache") is None)
    s, tog = call("/api/cache", {"enabled": True}, token=admin_tok, method="POST")
    check("cache toggle on", s == 200 and tog.get("enabled") is True)
    _, he, be = chat_raw(tok3, cache_q)
    check("re-enabled cache serves the kept entry", he.get("X-Agentgate-Cache") == "HIT" and be == b1)
    s, clr = call("/api/cache/clear", {}, token=admin_tok, method="POST")
    check("cache clear", s == 200 and clr.get("cleared", 0) >= 1)
    _, hc, _ = chat_raw(tok3, cache_q)
    check("cleared entry is a MISS again", hc.get("X-Agentgate-Cache") == "MISS")
    s, cache1 = call("/api/cache", token=admin_tok, method="GET")
    check("cache counters moved", s == 200
          and cache1.get("hits", 0) - cache0.get("hits", 0) >= 2
          and cache1.get("misses", 0) - cache0.get("misses", 0) >= 3)

    print(f"\n{len(failures)} failure(s)")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
