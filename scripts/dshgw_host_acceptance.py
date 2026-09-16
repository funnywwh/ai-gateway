#!/usr/bin/env python3
"""Explicit, staged M51 host acceptance (Python stdlib only).

baseline creates TWO disposable tenants and keeps them for manual/revocation
checks; resume-baseline rechecks those saved tenants without recreating them;
revoked only observes a key the operator has disabled; cleanup removes only
recorded, identity-matching tenants via dshgw's snapshot-first CLI.
report.json is shareable. state.json contains browser tokens: DO NOT SHARE IT.
This script never installs software, changes firewall/TLS/aigw configuration,
uses sudo, disables certificate verification, or deletes directories directly.
"""
import argparse
import base64
import hashlib
import html
import http.client
from http.cookies import SimpleCookie
import json
import os
from pathlib import Path
import pwd
import re
import secrets
import socket
import ssl
import stat
import subprocess
import sys
import time
from urllib.parse import urlencode, urlsplit

MAX_BODY = 4 << 20
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
IDENTITY_FIELDS = (
    "name", "uid", "user", "unit", "public_port", "worker_port", "created_at",
    "workspace", "dsh_home", "key_prefix", "previous_prefixes", "handshake_path",
    "gateway_key_path", "aigw_base_url", "key_revalidate", "worker_slice", "portal_url",
)
PENDING = ["external TLS/firewall probe", "10-30 worker resource/pressure test",
           "backup restore drill", "browser local-directory authorization", "real credential rotation"]


class Failure(Exception):
    """Only secret-free diagnostics may be put in this exception."""


def check(condition, message):
    if not condition:
        raise Failure(message)


def origin_parts(url):
    parsed = urlsplit(url)
    check(parsed.scheme in ("http", "https") and parsed.hostname and not parsed.username
          and not parsed.password and not parsed.query and not parsed.fragment,
          "invalid origin/base URL")
    try:
        port = parsed.port or (443 if parsed.scheme == "https" else 80)
    except ValueError:
        raise Failure("invalid URL port") from None
    return parsed, port


def origin_only(url):
    parsed, port = origin_parts(url)
    check(parsed.scheme == "https" and parsed.path in ("", "/"), "edge origin must be HTTPS without a path")
    host = f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
    return f"https://{host}:{port}"


def read_private(path, limit=MAX_BODY):
    path = Path(path)
    check(path.is_absolute(), "private file path must be absolute")
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as file:
            info = os.fstat(file.fileno())
            check(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600
                  and info.st_uid == os.geteuid(), "private file must be owner-owned, regular and mode 0600")
            data = file.read(limit + 1)
            check(len(data) <= limit, "private file exceeds size bound")
            return data
    except OSError:
        raise Failure("cannot securely read private file") from None


def read_key(path):
    try:
        key = read_private(path, 8192).decode("ascii").strip()
    except UnicodeError:
        raise Failure("key must be ASCII") from None
    if key.lower().startswith("bearer "):
        key = key[7:].strip()
    check(12 < len(key) <= 8192 and all(0x21 <= ord(c) <= 0x7e for c in key), "invalid test key")
    return key


def fingerprint(data):
    return hashlib.sha256(data).hexdigest()


def private_dir(path, create=False):
    path = Path(path)
    check(path.is_absolute() and str(path) == os.path.normpath(path), "run directory must be a clean absolute path")
    # A root report must never be writable through a tenant-owned ancestor.
    for ancestor in [path.parent, *path.parent.parents]:
        info = ancestor.lstat()
        check(stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode), "run directory has a symlink/non-directory ancestor")
        if os.geteuid() == 0:
            check(info.st_uid == 0 and not stat.S_IMODE(info.st_mode) & 0o022,
                  "root run directory ancestors must be root-owned and not group/world writable")
    if create:
        path.mkdir(mode=0o700)  # Exclusive: never reuse or overwrite an earlier run.
    info = path.lstat()
    check(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid()
          and stat.S_IMODE(info.st_mode) == 0o700, "run directory must be private mode 0700")
    return path


def atomic_json(path, data):
    path = Path(path)
    private_dir(path.parent)
    temporary = path.with_name("." + path.name + "." + secrets.token_hex(8))
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as file:
            json.dump(data, file, ensure_ascii=False, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path)
        dir_fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        if temporary.exists():
            temporary.unlink()


def run_command(argv, allowed=(0,), timeout=180):
    try:
        result = subprocess.run(argv, capture_output=True, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise Failure("command could not complete: " + Path(argv[0]).name) from None
    check(result.returncode in allowed, f"command failed: {Path(argv[0]).name} (exit {result.returncode}); inspect locally, do not share credential logs")
    # Never include arbitrary stdout/stderr in exceptions or public reports.
    return result


class CLI:
    def __init__(self, binary, config, runner=run_command):
        check(Path(binary).is_absolute() and Path(config).is_absolute(), "binary/config paths must be absolute")
        self.binary, self.config, self.runner = binary, config, runner
        self.audit_dir = None

    def call(self, *args, allowed=(0,)):
        result = self.runner([self.binary, "--config", self.config, *args], allowed=tuple(range(-128, 256)))
        if self.audit_dir is not None:
            private_dir(self.audit_dir)
            fd = os.open(Path(self.audit_dir) / "cli.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as file:
                info = os.fstat(file.fileno())
                check(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o600,
                      "CLI diagnostics must remain private")
                # argv contains file paths, never key values. Diagnostics are
                # PRIVATE and bounded; do not copy this log into public reports.
                json.dump({"args": list(args), "exit": result.returncode,
                           "stdout": result.stdout[-MAX_BODY:].decode("utf-8", errors="replace"),
                           "stderr": result.stderr[-MAX_BODY:].decode("utf-8", errors="replace")}, file)
                file.write("\n")
        check(result.returncode in allowed, f"dshgw {args[0]} failed (exit {result.returncode}); inspect private cli.log locally")
        return result

    def rows(self):
        try:
            rows = json.loads(self.call("tenant", "list", "--json", "--no-status").stdout)
        except (ValueError, UnicodeError):
            raise Failure("tenant list did not return valid JSON") from None
        check(isinstance(rows, list), "invalid tenant list")
        for row in rows:
            validate_row(row)
        return rows


def validate_row(row):
    check(isinstance(row, dict) and all(field in row for field in IDENTITY_FIELDS),
          "installed dshgw lacks M51 acceptance metadata; rebuild/install current binary")
    check(re.fullmatch(r"[a-z][a-z0-9-]{0,26}", row["name"]) is not None, "invalid tenant name")
    check(type(row["uid"]) is int and row["uid"] > 0, "invalid tenant UID")
    check(re.fullmatch(r"[a-z_][a-z0-9_-]{0,31}", row["user"]) is not None, "invalid tenant user")
    check(re.fullmatch(r"[A-Za-z0-9_.-]+@[a-z0-9-]+\.service", row["unit"]) is not None, "invalid tenant unit")
    check(row["unit"].split("@", 1)[1] == row["name"] + ".service", "unit does not identify tenant")
    for name in ("workspace", "dsh_home", "handshake_path", "gateway_key_path"):
        check(isinstance(row[name], str) and Path(row[name]).is_absolute()
              and os.path.normpath(row[name]) == row[name], "invalid managed path metadata")
    check(Path(row["workspace"]).name == row["name"] and Path(row["dsh_home"]).name == ".dsh"
          and Path(row["dsh_home"]).parent.name == row["name"], "tenant path metadata mismatch")
    for name in ("public_port", "worker_port"):
        check(type(row[name]) is int and 1 <= row[name] <= 65535, "invalid tenant port")
    origin_only(row["portal_url"])
    origin_parts(row["aigw_base_url"])


def identity(row):
    return {name: row[name] for name in IDENTITY_FIELDS}


def tenant_origin(row):
    portal, _ = origin_parts(row["portal_url"])
    return origin_only(f"https://{portal.hostname}:{row['public_port']}")


class Browser:
    def __init__(self, cookies=None):
        self.cookies = dict(cookies or {})

    def header(self):
        return "; ".join(f"{name}={value}" for name, value in sorted(self.cookies.items()))

    def accept(self, headers):
        for name, value in headers:
            check(name.lower() != "set-cookie2", "Set-Cookie2 leaked through the edge")
            if name.lower() != "set-cookie":
                continue
            parsed = SimpleCookie()
            parsed.load(value)
            check(bool(parsed), "malformed Set-Cookie")
            for cookie_name, cookie in parsed.items():
                check(not cookie_name.lower().startswith("dsh-auth-"), "worker authentication cookie leaked")
                if not cookie_name.startswith("dshgw_s_"):
                    continue
                check(cookie["secure"] and cookie["httponly"] and cookie["samesite"].lower() == "lax"
                      and cookie["path"] == "/" and not cookie["domain"], "gateway cookie lacks required attributes")
                if not cookie.value or cookie["max-age"] == "0":
                    self.cookies.pop(cookie_name, None)
                else:
                    self.cookies[cookie_name] = cookie.value


class Transport:
    def __init__(self, connect_address="127.0.0.1"):
        self.connect_address = connect_address

    def request(self, url, method="GET", body=None, headers=None, browser=None, edge=True):
        parsed, port = origin_parts(url.split("?", 1)[0])
        target = urlsplit(url)
        path = target.path or "/"
        if target.query:
            path += "?" + target.query
        host = parsed.hostname
        connection = (http.client.HTTPSConnection(host, port, timeout=45, context=ssl.create_default_context())
                      if parsed.scheme == "https" else http.client.HTTPConnection(host, port, timeout=45))
        if edge and self.connect_address:
            check(parsed.scheme == "https", "edge requests require HTTPS")
            raw = socket.create_connection((self.connect_address, port), timeout=45)
            try:
                connection.sock = ssl.create_default_context().wrap_socket(raw, server_hostname=host)
            except BaseException:
                raw.close()
                raise
        request_headers = {"Host": f"{host}:{port}", **(headers or {})}
        if browser and browser.cookies and "Cookie" not in request_headers:
            request_headers["Cookie"] = browser.header()
        try:
            connection.request(method, path, body=body, headers=request_headers)
            response = connection.getresponse()
            response_headers = response.getheaders()
            if browser:
                browser.accept(response_headers)
            else:
                Browser().accept(response_headers)
            data = response.read(MAX_BODY + 1)
            check(len(data) <= MAX_BODY, "HTTP response exceeded size limit")
            return response.status, dict((k.lower(), v) for k, v in response_headers), data
        finally:
            connection.close()

    def websocket(self, origin, browser, path="/api/remote.mux", send_origin=True):
        return MuxSocket(origin, browser, path, self.connect_address, send_origin)


class MuxSocket:
    """Bounded RFC6455 upgrade probe; closes without exchanging model data."""
    def __init__(self, origin, browser, path, connect_address, send_origin):
        parsed, port = origin_parts(origin)
        check(parsed.scheme == "https", "WebSocket edge requires TLS")
        self.sock = socket.create_connection((connect_address or parsed.hostname, port), timeout=30)
        self.response = None
        try:
            self.sock = ssl.create_default_context().wrap_socket(self.sock, server_hostname=parsed.hostname)
            nonce = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
            fields = [f"GET {path} HTTP/1.1", f"Host: {parsed.hostname}:{port}", "Connection: Upgrade",
                      "Upgrade: websocket", "Sec-WebSocket-Version: 13", f"Sec-WebSocket-Key: {nonce}",
                      "Cookie: " + browser.header()]
            if send_origin:
                fields.append("Origin: " + origin)
            self.sock.sendall(("\r\n".join(fields) + "\r\n\r\n").encode("ascii"))
            response = self.response = http.client.HTTPResponse(self.sock)
            response.begin()
            browser.accept(response.getheaders())
            self.status = response.status
            check(self.status == 101, f"WebSocket upgrade returned {self.status}, expected 101")
            expected = base64.b64encode(hashlib.sha1((nonce + WS_GUID).encode()).digest()).decode()
            check(response.getheader("Sec-WebSocket-Accept") == expected, "invalid WebSocket accept hash")
        except BaseException:
            self.close()
            raise

    def close(self):
        # HTTPResponse owns fp. Closing fp directly leaves response.closed false
        # and Python 3.14's finalizer later tries to flush that closed stream.
        try:
            if self.response is not None:
                self.response.close()
        finally:
            self.response = None
            self.sock.close()



def rpc(transport, origin, browser, method, request):
    envelope = {"type": "client-request", "rpcId": secrets.token_hex(8), "method": method,
                "payload": {"args": {"_request" if method == "session/list" else "request": request}}}
    status, _, body = transport.request(origin + "/api/" + method, "POST", json.dumps(envelope).encode(),
                                       {"Origin": origin, "Content-Type": "application/json"}, browser)
    check(status == 200, f"worker RPC {method} returned {status}")
    try:
        response = json.loads(body)
    except ValueError:
        raise Failure("worker RPC did not return JSON") from None
    check(response.get("type") == "server-response" and response.get("rpcId") == envelope["rpcId"]
          and response.get("result", {}).get("ok") is True, f"worker RPC {method} was rejected")
    return response["result"].get("value")


def completed_turn(records, request_id):
    events = sorted((record["event"] for record in records if record.get("type") == "event"), key=lambda event: event["seq"])
    current_turn = target_turn = None
    saw_text = False
    for event in events:
        data = event.get("data", {})
        if event.get("type") == "turn/start":
            current_turn = data.get("turn")
        if event.get("type") == "user/message" and data.get("source", {}).get("rpcId") == request_id:
            check(type(current_turn) is int, "prompt cursor page lacks its turn/start correlation")
            target_turn = current_turn
            continue
        if target_turn is None:
            continue
        if event.get("type") == "tool/call":
            raise Failure("unexpected model tool use; stop this disposable smoke session")
        if event.get("type") == "assistant/message" and data.get("turn") == target_turn and not data.get("interrupted", False):
            saw_text = saw_text or any(block.get("type") == "text" and block.get("text", "").strip()
                                      for block in data.get("message", {}).get("content", []))
        if event.get("type") == "turn/end" and data.get("turn") == target_turn:
            check(data.get("reason", {}).get("kind") == "completed" and saw_text,
                  "worker model turn did not complete with an assistant reply")
            return True
    return False


def prompt_worker(transport, row, browser, model):
    origin = tenant_origin(row)
    created = rpc(transport, origin, browser, "session/create", {"cwd": row["workspace"]})
    check(isinstance(created, dict) and isinstance(created.get("sessionId"), str), "worker did not create a session")
    session_id = created["sessionId"]
    rpc(transport, origin, browser, "session/selectModel", {"sessionId": session_id, "provider": "aigw", "model": model})
    # Pin a user title before prompting so the auxiliary title LLM is not run.
    # rename always appends a public title event and returns its exact seq; it
    # supplies a durable cursor fence without scraping internal files/errors.
    title = "M51 disposable smoke"
    rpc(transport, origin, browser, "session/rename", {"sessionId": session_id, "title": title})
    request_id = "m51-" + secrets.token_hex(8)
    finished = False
    try:
        rpc(transport, origin, browser, "session/prompt", {"requestId": request_id,
            "sessionId": session_id, "mode": "queue", "content": [{"type": "text", "text": "Reply exactly OK. Do not use any tools."}]})
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            listing = rpc(transport, origin, browser, "session/list", {})
            matches = [item for item in listing.get("items", []) if item.get("sessionId") == session_id]
            check(len(matches) == 1, "smoke session missing from public listing")
            if not matches[0].get("running", False):
                fence = rpc(transport, origin, browser, "session/rename", {"sessionId": session_id, "title": title})
                check(type(fence.get("seq")) is int and fence["seq"] >= 0, "worker did not return a public cursor fence")
                page = rpc(transport, origin, browser, "session/page", {"address": {"kind": "session", "sessionId": session_id}, "throughSeq": fence["seq"], "maxMessages": 20})
                if completed_turn(page.get("records", []), request_id):
                    finished = True
                    return {"session_id": session_id, "completed": True, "has_reply": True}
            time.sleep(0.25)
        raise Failure("worker model turn timed out")
    finally:
        if not finished:
            try:
                rpc(transport, origin, browser, "session/cancel", {"sessionId": session_id})
            except (Failure, OSError, ValueError):
                # baseline's failure handler also stops identity-matching test
                # units; it does not silently leave chargeable turns running.
                pass


def assert_identity(saved, current):
    check(identity(saved) == identity(current), "tenant identity/config changed; refusing to operate on it")


def unit_properties(row, runner=run_command):
    names = ("User", "MainPID", "ActiveState", "ControlGroup", "ProtectHome", "PrivateTmp", "Slice")
    output = runner(["systemctl", "show", "--property=" + ",".join(names), row["unit"]]).stdout.decode()
    props = dict(line.split("=", 1) for line in output.splitlines() if "=" in line)
    check(props.get("User") == row["user"] and props.get("ActiveState") == "active", "worker systemd identity/activity mismatch")
    check(props.get("ProtectHome") == "tmpfs" and props.get("PrivateTmp") in ("yes", "disconnected")
          and props.get("Slice") == row["worker_slice"], "worker isolation properties mismatch")
    check(props.get("MainPID", "").isdigit() and int(props["MainPID"]) > 0, "worker MainPID unavailable")
    check(pwd.getpwnam(row["user"]).pw_uid == row["uid"], "OS user UID differs from registry")
    status = Path("/proc", props["MainPID"], "status").read_text()
    match = re.search(r"^Uid:\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)", status, re.M)
    check(match and all(int(v) == row["uid"] for v in match.groups()), "worker process runs as wrong UID")
    cg = props.get("ControlGroup", "")
    check(cg.startswith("/") and ".." not in Path(cg).parts, "unsafe cgroup path")
    root = Path("/sys/fs/cgroup")
    directory = root / cg.lstrip("/")
    check((root / "cgroup.controllers").is_file(), "this automatic resource check requires cgroup v2")
    limits = {name: (directory / name).read_text().strip() for name in ("memory.max", "memory.high", "pids.max", "cpu.max")}
    check(limits["memory.max"] == str(2 << 30) and limits["memory.high"] == str(1536 << 20)
          and limits["pids.max"] == "512", "worker cgroup limits differ from M51 defaults")
    quota, period = limits["cpu.max"].split()
    check(quota != "max" and int(quota) == 2 * int(period), "worker CPU quota is not 200 percent")
    check(directory.parent.name == row["worker_slice"] and (directory.parent / "memory.max").read_text().strip() == str(40 << 30),
          "aggregate worker slice is not capped at 40 GiB")
    sockets = runner(["ss", "-H", "-ltn", f"sport = :{row['worker_port']}"]).stdout.decode()
    listeners = [line.split()[3] for line in sockets.splitlines() if len(line.split()) >= 4]
    check(listeners == [f"127.0.0.1:{row['worker_port']}"], "worker listener is missing or exposed outside IPv4 loopback")
    rss = re.search(r"^VmRSS:\s+(\d+) kB", status, re.M)
    return {"pid": int(props["MainPID"]), "uid": row["uid"], "rss_kib": int(rss[1]) if rss else None,
            "cgroup": cg, "limits": limits, "pressure_tested": False}


def file_checks(rows, runner=run_command):
    check(rows[0]["uid"] != rows[1]["uid"], "tenants share an OS UID")
    for row in rows:
        for path, mode, uid in ((row["workspace"], 0o700, row["uid"]),
                                (str(Path(row["dsh_home"]) / ".credentials.yaml"), 0o600, row["uid"]),
                                (row["handshake_path"], 0o640, 0), (row["gateway_key_path"], 0o640, 0)):
            info = os.lstat(path)
            check(not stat.S_ISLNK(info.st_mode) and stat.S_IMODE(info.st_mode) == mode and info.st_uid == uid,
                  "tenant managed path mode/owner mismatch")
    # Check actual EACCES, not a missing path or an arbitrary command failure.
    code = "import os,sys\ntry:\n f=os.open(sys.argv[1],os.O_RDONLY);os.close(f)\nexcept PermissionError:sys.exit(77)\nexcept OSError:sys.exit(78)\n"
    for own, other in ((rows[0], rows[1]), (rows[1], rows[0])):
        runner(["runuser", "-u", own["user"], "--", sys.executable, "-c", code,
                str(Path(own["dsh_home"]) / ".credentials.yaml")])
        for path in (other["workspace"], str(Path(other["dsh_home"]) / ".credentials.yaml")):
            runner(["runuser", "-u", own["user"], "--", sys.executable, "-c", code, path], allowed=(77,))


class Acceptance:
    def __init__(self, cli, directory, transport=None):
        self.cli, self.directory = cli, Path(directory)
        self.cli.audit_dir = self.directory
        self.transport = transport or Transport()
        self.state = {}
        self.report = {"schema": 1, "overall_complete": False, "checks": [], "pending": list(PENDING)}

    def save(self):
        atomic_json(self.directory / "state.json", self.state)
        atomic_json(self.directory / "report.json", self.report)

    def passed(self, name, detail=None):
        self.report["checks"].append({"check": name, "status": "passed", "detail": detail})
        self.save()
        print("PASS", name)

    def request(self, origin, path="/", method="GET", body=None, headers=None, browser=None, expected=(200,)):
        status, response_headers, data = self.transport.request(origin.rstrip("/") + path, method, body, headers, browser)
        check(status in expected, f"{method} {path.split('?', 1)[0]} returned {status}, expected {expected}")
        return response_headers, data

    def login(self, row, key, browser):
        portal, origin = origin_only(row["portal_url"]), tenant_origin(row)
        headers, _ = self.request(portal, "/login", "POST", urlencode({"key": key}).encode(),
                                 {"Origin": portal, "Content-Type": "application/x-www-form-urlencoded"}, browser, (302,))
        check(headers.get("location") == origin + "/" and "dshgw_s_" + row["name"] in browser.cookies,
              "portal login did not issue the correct tenant session")

    def validate_live_key(self, row, key, model, expected=200):
        base = row["aigw_base_url"].rstrip("/")
        for header in ({"Authorization": "Bearer " + key}, {"X-API-Key": key}):
            status, _, body = self.transport.request(base + "/v1/models", headers=header, edge=False)
            check(status == expected, f"aigw model-list authentication returned {status}, expected {expected}")
            if expected == 200:
                try:
                    ids = {entry["id"] for entry in json.loads(body)["data"]}
                except (KeyError, TypeError, ValueError):
                    raise Failure("invalid aigw model list") from None
                check(model in ids, "requested test model is not granted/available to both keys")

    def baseline(self, key_paths, model, aigw_url):
        keys = [read_key(path) for path in key_paths]
        check(keys[0][:12] != keys[1][:12], "test keys must have distinct prefixes")
        before = self.cli.rows()
        prefixes = {p for row in before for p in [row["key_prefix"], *row["previous_prefixes"]]}
        check(all(key[:12] not in prefixes for key in keys), "a test key is already bound; use dedicated unbound keys")
        portal = origin_only(self.cli.call("login-url").stdout.decode().strip())
        self.cli.call("doctor")
        self.request(portal)
        for path, key in zip(key_paths, keys):
            self.cli.call("contract", "--key-file", path, "aigw")
            self.validate_live_key({"aigw_base_url": aigw_url}, key, model)
        run_id = secrets.token_hex(4)
        self.state = {"schema": 1, "binary": self.cli.binary, "config": self.cli.config,
                      "config_sha256": fingerprint(Path(self.cli.config).read_bytes()), "run_id": run_id,
                      "keys": [{"path": path, "sha256": fingerprint(key.encode())} for path, key in zip(key_paths, keys)],
                      "model": model, "before": [identity(row) for row in before], "attempted": [], "tenants": [], "cookies": {}}
        self.report.update(phase="baseline", status="running", portal_url=portal + "/", tenants=[])
        self.save()
        try:
            for suffix, path in zip(("a", "b"), key_paths):
                name = f"m51-e2e-{run_id}-{suffix}"
                check(all(row["name"] != name for row in before), "test tenant name already exists")
                self.state["attempted"].append(name)
                self.save()
                self.cli.call("tenant", "create", "--browser-fs", "on", "--key-file", path, name)
                rows = [row for row in self.cli.rows() if row["name"] == name]
                check(len(rows) == 1, "created tenant missing from registry")
                self.state["tenants"].append(rows[0])
                self.report["tenants"].append({"name": name, "url": tenant_origin(rows[0]) + "/"})
                self.save()
            self._verify_baseline(self.state["tenants"], keys, model, portal, aigw_url)
        except BaseException:
            self._baseline_failed()
            raise

    def _verify_baseline(self, rows, keys, model, portal, aigw_url):
        """Verify existing tenants; creation/resume ownership belongs to callers."""
        check(rows[0]["aigw_base_url"].rstrip("/") == rows[1]["aigw_base_url"].rstrip("/") == aigw_url.rstrip("/"), "test tenants do not target the supplied aigw")
        for row, key in zip(rows, keys):
            self.validate_live_key(row, key, model)
        self.passed("real-aigw-key-headers-and-model-grants")
        file_checks(rows)
        self.passed("two-distinct-uids-and-cross-tenant-EACCES")
        for row in rows:
            self.passed("worker-cgroup-" + row["name"], unit_properties(row))
        browser = Browser()
        for row, key in zip(rows, keys):
            self.login(row, key, browser)
            _, page = self.request(tenant_origin(row), browser=browser)
            match = re.search(rb'/plugins/\?\?[^"\s<>]*dsh-browser-fs/client\.js[^"\s<>]*', page)
            check(match is not None, "worker page lacks advertised browser-fs client")
            self.request(tenant_origin(row), html.unescape(match[0].decode()), browser=browser)
            self.request(tenant_origin(row), "/browser-fs/ws", browser=browser, expected=(426,))
            websocket = self.transport.websocket(tenant_origin(row), browser, "/browser-fs/ws")
            websocket.close()
        self.passed("portal-login-client-200-and-browser-fs-426-101")
        a, b = rows
        for target, source in ((a, b), (b, a)):
            origin = tenant_origin(target)
            self.request(origin, "/api/session/list", "POST", b"{}", {"Origin": tenant_origin(source)}, browser, (403,))
            self.request(origin, "/browser-fs/ws", headers={"Origin": tenant_origin(source), "Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}, browser=browser, expected=(403,))
        foreign = Browser({"dshgw_s_" + a["name"]: browser.cookies["dshgw_s_" + b["name"]]})
        self.request(tenant_origin(a), browser=foreign, expected=(302,))
        self.passed("cross-port-HTTP-WS-and-cookie-tenant-binding")
        for row in rows:
            self.passed("real-worker-model-turn-" + row["name"], prompt_worker(self.transport, row, browser, model))
        old_pid = unit_properties(a)["pid"]
        old_handshake = fingerprint(Path(a["handshake_path"]).read_bytes())
        self.cli.call("tenant", "restart", a["name"])
        check(unit_properties(a)["pid"] != old_pid and fingerprint(Path(a["handshake_path"]).read_bytes()) != old_handshake,
              "worker restart did not replace PID/handshake")
        rpc(self.transport, tenant_origin(a), browser, "session/create", {"cwd": a["workspace"]})
        self.passed("restart-and-worker-cookie-self-heal")
        old_browser = Browser(browser.cookies)
        other_browser = Browser()
        self.login(a, keys[0], other_browser)
        self.request(portal, "/logout", "POST", b"", {"Origin": portal}, browser, (303,))
        for row in rows:
            self.request(tenant_origin(row), browser=old_browser, expected=(302,))
        self.request(tenant_origin(a), browser=other_browser)
        self.login(b, keys[1], other_browser)
        self.state["cookies"] = other_browser.cookies
        self.passed("logout-revokes-browser-sessions-not-other-browser")
        self.report["status"] = "passed"
        self.report["next"] = "Disable ONLY test Key A in aigw, then run revoked. Do not share state.json."
        self.save()

    def _baseline_failed(self):
        """Stop only still-identity-matching recorded workers; never purge data."""
        self.report["status"] = "failed"
        self.report["next"] = "Test resources retained; inspect locally and run snapshot-first cleanup."
        warnings = self.report.setdefault("stop_warnings", [])
        allowed_names = {f"m51-e2e-{self.state['run_id']}-{suffix}" for suffix in ("a", "b")}
        for saved in self.state["tenants"]:
            try:
                check(saved["name"] in allowed_names and saved["name"] in self.state["attempted"],
                      "refusing to stop an unowned tenant")
                validate_row(saved)
                current = {row["name"]: row for row in self.cli.rows()}
                check(saved["name"] in current, "test tenant vanished")
                assert_identity(saved, current[saved["name"]])
                run_command(["systemctl", "stop", saved["unit"]], timeout=60)
            except (Failure, OSError, ValueError, KeyError, TypeError):
                warnings.append("Could not confirm stop for " + saved["name"] + "; inspect immediately")
        self.save()

    def resume_baseline(self, *, confirm_start_workers=False, allow_model_call=False):
        """Resume an incomplete saved baseline; never create or remove tenants."""
        check(confirm_start_workers and allow_model_call,
              "resume-baseline requires --confirm-start-workers --allow-model-call")
        # Check file/config/key fingerprints BEFORE any CLI/systemd command.
        # load() below remains the single shared current-identity guard and its
        # public API stays unchanged for revoked/cleanup/diagnostic consumers.
        self._load_documents()
        saved_state, saved_report = self.state, self.report
        check(saved_report.get("schema") == 1
              and saved_report.get("phase") in ("baseline", "resume-baseline")
              and saved_report.get("status") in ("failed", "running"),
              "resume-baseline requires an incomplete baseline, not a revoked or cleaned run")
        check(isinstance(saved_report.get("checks"), list)
              and isinstance(saved_report.get("stop_warnings", []), list)
              and isinstance(saved_report.get("resume_attempts", []), list),
              "invalid saved baseline evidence")
        expected = [f"m51-e2e-{saved_state['run_id']}-{suffix}" for suffix in ("a", "b")]
        rows = saved_state.get("tenants")
        check(isinstance(rows, list) and len(rows) == 2, "resume requires exactly two recorded test tenants")
        for row in rows:
            validate_row(row)
        check([row["name"] for row in rows] == expected and saved_state.get("attempted") == expected,
              "resume requires exactly the original ordered test tenants; refusing unknown names")
        model = saved_state.get("model")
        check(isinstance(model, str) and bool(model.strip()), "saved baseline model is missing")
        key_entries = saved_state.get("keys")
        check(isinstance(key_entries, list) and len(key_entries) == 2
              and all(isinstance(entry, dict) and isinstance(entry.get("path"), str)
                      and isinstance(entry.get("sha256"), str) for entry in key_entries),
              "resume requires the two original saved key files")
        keys = [read_key(entry["path"]) for entry in key_entries]
        check(all(fingerprint(key.encode()) == entry["sha256"] for key, entry in zip(keys, key_entries)),
              "key file changed since baseline; refusing to start workers")
        check(keys[0][:12] != keys[1][:12]
              and all(key[:12] == row["key_prefix"] for row, key in zip(rows, keys)),
              "saved test keys no longer identify their recorded tenants")
        portal = origin_only(saved_report.get("portal_url", ""))
        check(all(origin_only(row["portal_url"]) == portal for row in rows), "saved portal metadata changed")
        aigw_url = rows[0]["aigw_base_url"].rstrip("/")
        check(rows[1]["aigw_base_url"].rstrip("/") == aigw_url, "saved test tenants target different aigw instances")
        current = self.load()
        check(self.state == saved_state and self.report == saved_report, "saved run changed during resume preflight")
        check(all(name in current for name in expected), "resume requires both recorded test tenants to exist")
        check({name for name in current if name.startswith(f"m51-e2e-{saved_state['run_id']}-")} == set(expected),
              "unrecorded tenant exists for this run; refusing to resume")
        # Read and validate BOTH units before starting either. A failed,
        # activating/deactivating, unloaded, or identity-mismatched unit needs
        # operator diagnosis, not an implicit recovery attempt.
        states = []
        for row in rows:
            try:
                account = pwd.getpwnam(row["user"])
            except KeyError:
                raise Failure("recorded worker OS identity is missing") from None
            check(account.pw_uid == row["uid"], "recorded worker OS UID changed")
            output = run_command(["systemctl", "show", "--property=Id,LoadState,ActiveState,SubState,User,MainPID",
                                  row["unit"]], timeout=30).stdout.decode()
            lines = output.splitlines()
            check(all("=" in line for line in lines), "invalid worker unit state response")
            props = dict(line.split("=", 1) for line in lines)
            check(len(props) == len(lines) and props.get("Id") == row["unit"]
                  and props.get("User") == row["user"] and props.get("LoadState") == "loaded",
                  "recorded worker unit identity/load state changed")
            state, substate, pid = props.get("ActiveState"), props.get("SubState"), props.get("MainPID", "")
            check((state == "inactive" and substate == "dead" and pid == "0")
                  or (state == "active" and substate == "running" and pid.isdigit() and int(pid) > 1),
                  "worker must be active/running or inactive/dead; diagnose transitional/unknown state first")
            states.append(state)
        attempt = {"started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "status": "running",
                   "previous_phase": saved_report["phase"], "previous_status": saved_report["status"],
                   "previous_next": saved_report.get("next"), "check_offset": len(self.report["checks"]),
                   "start_attempted": [], "started_units": []}
        self.report.setdefault("resume_attempts", []).append(attempt)
        self.report.update(phase="resume-baseline", status="running")
        self.save()
        try:
            for row, state in zip(rows, states):
                if state == "inactive":
                    attempt["start_attempted"].append(row["unit"])
                    self.save()  # Retain a possibly-partial start even if systemctl fails.
                    run_command(["systemctl", "start", row["unit"]], timeout=60)
                    attempt["started_units"].append(row["unit"])
                    self.save()
            self._verify_baseline(rows, keys, model, portal, aigw_url)
            attempt.update(status="passed", finished_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
            self.save()
        except BaseException:
            attempt.update(status="failed", finished_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
            self._baseline_failed()
            raise

    def _load_documents(self):
        private_dir(self.directory)
        self.state = json.loads(read_private(self.directory / "state.json"))
        self.report = json.loads(read_private(self.directory / "report.json"))
        check(self.state.get("schema") == 1 and re.fullmatch(r"[0-9a-f]{8}", self.state.get("run_id", "")), "invalid acceptance state")
        check(self.cli.binary == self.state["binary"] and self.cli.config == self.state["config"], "binary/config differs from saved run")
        check(fingerprint(Path(self.cli.config).read_bytes()) == self.state["config_sha256"], "configuration changed since baseline")

    def load(self):
        self._load_documents()
        current = {row["name"]: row for row in self.cli.rows()}
        allowed_names = {f"m51-e2e-{self.state['run_id']}-{suffix}" for suffix in ("a", "b")}
        check(set(self.state["attempted"]) <= allowed_names, "state contains unowned tenant names")
        for row in self.state["tenants"]:
            validate_row(row)
            check(row["name"] in self.state["attempted"] and row["name"].startswith("m51-e2e-" + self.state["run_id"] + "-"), "refusing non-test tenant")
            if row["name"] in current:
                assert_identity(row, current[row["name"]])
        for row in self.state["before"]:
            check(row["name"] in current and identity(current[row["name"]]) == row, "pre-existing tenant identity changed; stop and inspect")
        return current

    def revoked(self):
        current = self.load()
        check(len(self.state["tenants"]) == 2 and all(row["name"] in current for row in self.state["tenants"]), "baseline tenants are incomplete")
        keys = [read_key(entry["path"]) for entry in self.state["keys"]]
        check(all(fingerprint(key.encode()) == entry["sha256"] for key, entry in zip(keys, self.state["keys"])), "key file changed; refusing ambiguous revocation test")
        a, b = self.state["tenants"]
        self.validate_live_key(a, keys[0], self.state["model"], expected=401)
        self.validate_live_key(b, keys[1], self.state["model"])
        status, _, _ = self.transport.request(a["aigw_base_url"].rstrip("/") + "/v1/responses", "POST",
            json.dumps({"model": self.state["model"], "input": "Reply OK", "stream": False, "max_output_tokens": 32}).encode(),
            {"Authorization": "Bearer " + keys[0], "Content-Type": "application/json"}, edge=False)
        check(status == 401, "disabled Key A still reached model generation")
        self.cli.call("revalidate", a["name"], allowed=(1,))
        self.cli.call("revalidate", b["name"])
        browser = Browser(self.state["cookies"])
        self.request(tenant_origin(b), browser=browser)
        mode = a["key_revalidate"]
        if mode == "off":
            self.request(tenant_origin(a), browser=browser)
        elif mode == "per-request":
            self.request(tenant_origin(a), browser=browser, expected=(302,))
        elif mode.startswith("interval:"):
            # Never wait indefinitely or alter production policy to speed this up.
            status, _, _ = self.transport.request(tenant_origin(a), browser=browser)
            check(status == 302, "interval cache not expired yet; rerun revoked after the configured interval")
        else:
            raise Failure("unknown revalidation policy")
        self.state["cookies"] = browser.cookies
        self.report.update(phase="revoked", status="passed", next="Complete outstanding manual checks, then run cleanup.")
        self.passed("disabled-key-model-401-and-configured-session-policy", {"mode": mode})

    def cleanup(self):
        current = self.load()
        snapshots = self.report.setdefault("snapshots", [])
        known = {row["name"] for row in self.state["tenants"]}
        check(all(name in known or name not in current for name in self.state["attempted"]),
              "an attempted create has an unrecorded tenant; inspect manually, do not blindly purge")
        for row in reversed(self.state["tenants"]):
            if row["name"] not in current:
                # Do not claim orphan cleanup: a failed create/remove may retain an OS identity.
                try:
                    pwd.getpwnam(row["user"])
                except KeyError:
                    pass
                else:
                    raise Failure("unregistered test OS user remains; manual inspection required")
                check(not any(os.path.lexists(path) for path in (row["workspace"], str(Path(row["dsh_home"]).parent),
                          str(Path(row["gateway_key_path"]).parent), row["handshake_path"])),
                      "unregistered test data remains; manual inspection required")
                continue
            output = self.cli.call("tenant", "remove", "--purge", "--yes", row["name"]).stdout.decode()
            match = re.search(r"snapshot: (/.+)\s*$", output)
            check(match is not None, "removal did not return its snapshot path")
            snapshots.append({"tenant": row["name"], "path": match[1].strip()})
            self.save()
        remaining = {row["name"]: row for row in self.cli.rows()}
        check(not any(name in remaining for name in self.state["attempted"]), "test routing entries remain")
        for row in self.state["before"]:
            check(row["name"] in remaining and identity(remaining[row["name"]]) == row, "pre-existing tenant changed during cleanup")
        check(set(self.state["attempted"]) <= known,
              "failed create attempts lack recorded identities; known tenants cleaned, inspect unrecorded attempts manually")
        self.state["cookies"] = {}
        self.report.update(phase="cleanup", status="passed", next="Revoke the dedicated test keys; retain private snapshots per policy.")
        self.passed("snapshot-first-cleanup-preserves-existing-tenant-identities")


def external_probe(report_path, transport=None):
    # The public report contains no browser tokens or keys and may be copied to
    # another machine. Never use the private state file for an external probe.
    report = json.loads(Path(report_path).read_text())
    check(report.get("schema") == 1 and len(report.get("tenants", [])) == 2
          and "keys" not in report and "cookies" not in report, "external probe needs a two-tenant PUBLIC report, never private state")
    transport = transport or Transport(connect_address=None)
    portal = origin_only(report["portal_url"])
    result = {"schema": 1, "phase": "external", "overall_complete": False, "checks": []}
    for url, expected in [(portal, 200), *[(origin_only(row["url"]), 302) for row in report["tenants"]]]:
        status, headers, _ = transport.request(url + "/")
        check(status == expected, f"external HTTPS probe returned {status}, expected {expected}")
        if expected == 302:
            check(headers.get("location") == portal + "/", "external tenant did not redirect to the portal")
        result["checks"].append({"url": url, "status": status, "tls_verified": True})
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("baseline", "resume-baseline", "revoked", "cleanup", "external"))
    parser.add_argument("--binary", default="/opt/dshgw/bin/dshgw")
    parser.add_argument("--config", default="/etc/dshgw/config.yaml")
    parser.add_argument("--run-dir", help="new private root directory for baseline; same directory for later phases")
    parser.add_argument("--key-a")
    parser.add_argument("--key-b")
    parser.add_argument("--model")
    parser.add_argument("--aigw-url", help="explicit target for read-only preflight model grants; must match configured gateway")
    parser.add_argument("--confirm-external-machine", action="store_true")
    parser.add_argument("--confirm-create", action="store_true")
    parser.add_argument("--confirm-start-workers", action="store_true",
                        help="resume only the saved test workers; never create or remove tenants")
    parser.add_argument("--allow-model-call", action="store_true", help="permit two short DSH model turns; their disposable titles are pinned to avoid title LLM calls")
    parser.add_argument("--confirm-key-disabled", action="store_true")
    parser.add_argument("--confirm-cleanup", action="store_true")
    parser.add_argument("--report", help="public report copied to a different machine for external probes")
    args = parser.parse_args(argv)
    try:
        if args.phase == "external":
            check(args.report and args.confirm_external_machine, "external requires --report and --confirm-external-machine on a different machine")
            print(json.dumps(external_probe(args.report), ensure_ascii=False, indent=2))
            return 0
        check(os.geteuid() == 0, "run this phase in the operator's root terminal; this script never uses sudo")
        check(args.run_dir, "--run-dir is required")
        directory = Path(args.run_dir)
        if args.phase == "baseline":
            check(args.confirm_create and args.allow_model_call, "baseline requires --confirm-create --allow-model-call")
            check(args.key_a and args.key_b and args.model and args.aigw_url, "baseline requires two key files, a real model ID, and --aigw-url")
            directory = private_dir(directory, create=True)
        elif args.phase == "resume-baseline":
            check(args.confirm_start_workers and args.allow_model_call,
                  "resume-baseline requires --confirm-start-workers --allow-model-call")
            check(not any(value is not None for value in (args.key_a, args.key_b, args.model, args.aigw_url)),
                  "resume-baseline uses the original saved key files/model/endpoint; omit their overrides")
            private_dir(directory)
        else:
            private_dir(directory)
        engine = Acceptance(CLI(args.binary, args.config), directory)
        if args.phase == "baseline":
            engine.baseline([args.key_a, args.key_b], args.model, args.aigw_url)
        elif args.phase == "resume-baseline":
            engine.resume_baseline(confirm_start_workers=args.confirm_start_workers, allow_model_call=args.allow_model_call)
        elif args.phase == "revoked":
            check(args.confirm_key_disabled, "disable ONLY test Key A in aigw, then pass --confirm-key-disabled")
            engine.revoked()
        else:
            check(args.confirm_cleanup, "cleanup requires --confirm-cleanup and snapshots will contain test keys")
            engine.cleanup()
        print("Share only:", directory / "report.json")
        print("Private bearer state (DO NOT SHARE):", directory / "state.json")
        return 0
    except (Failure, OSError, ValueError, KeyError, TypeError) as error:
        # Do not interpolate arbitrary protocol bodies, key material, or OS argv.
        print("FAIL:", str(error) if isinstance(error, Failure) else type(error).__name__ + "; inspect host locally", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
