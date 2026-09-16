#!/usr/bin/env python3
"""Exercise the host acceptance worker protocol without root or host services.

This test starts the installed DSH on a disposable HOME, puts a fake OpenAI
Responses endpoint on loopback, and drives the same public RPC helpers used by
``dshgw_host_acceptance.py``.  It deliberately does not contact the gateway,
port 3080, a systemd unit, or a real model provider.
"""
from __future__ import annotations

import http.client
import importlib.util
import json
import os
from pathlib import Path
import queue
import re
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlsplit


NODE = os.environ.get("DSHGW_NODE")
DSH_ROOT = os.environ.get("DSHGW_DSH_ROOT")
if not NODE or not DSH_ROOT:
    print("SKIP: DSHGW_NODE and DSHGW_DSH_ROOT are required")
    raise SystemExit(0)

node_path = Path(NODE)
dsh_root = Path(DSH_ROOT)
dsh_bin = dsh_root / "lib" / "bin.js"
if not node_path.is_file() or not os.access(node_path, os.X_OK):
    raise SystemExit(f"FAIL: DSHGW_NODE is not an executable file: {node_path}")
if not dsh_bin.is_file():
    raise SystemExit(f"FAIL: DSHGW_DSH_ROOT has no lib/bin.js: {dsh_bin}")

ROOT = Path(__file__).resolve().parents[1]
HOST_ACCEPTANCE = ROOT / "scripts" / "dshgw_host_acceptance.py"
_spec = importlib.util.spec_from_file_location("dshgw_host_acceptance", HOST_ACCEPTANCE)
if _spec is None or _spec.loader is None:
    raise SystemExit("FAIL: cannot import dshgw_host_acceptance.py")
_host = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_host)

Browser = _host.Browser
Failure = _host.Failure
prompt_worker = _host.prompt_worker

MODEL = "dshgw-host-protocol-probe"
KEY = "sk-dshgw-host-protocol-dummy"
MAX_BODY = 8 << 20
STARTUP_RE = re.compile(r"^dsh web: (http://127\.0\.0\.1:\d+/\?token=[A-Za-z0-9_-]{43})(?: \(LAN: .+\))?$")


class FakeResponses:
    """A bounded fake streaming Responses backend on 127.0.0.1."""

    def __init__(self):
        self.requests: list[dict] = []
        self.errors: list[str] = []
        self._lock = threading.Lock()
        self.server = None

    def start(self):
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        owner = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, _format, *_args):
                return

            def fail(self, message):
                with owner._lock:
                    owner.errors.append(message)
                body = b"fake provider contract failed"
                self.send_response(500)
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.wfile.write(body)
                self.close_connection = True

            def do_POST(self):  # noqa: N802 - stdlib HTTP handler API
                expected_host = f"127.0.0.1:{owner.server.server_port}"
                if self.path != "/v1/responses":
                    self.fail("unexpected fake endpoint path")
                    return
                if self.headers.get("Host") != expected_host:
                    self.fail("unexpected fake endpoint authority")
                    return
                try:
                    size = int(self.headers.get("Content-Length", "-1"))
                    if size < 0 or size > MAX_BODY:
                        raise ValueError("invalid request size")
                    payload = json.loads(self.rfile.read(size))
                    if payload.get("model") != MODEL or payload.get("stream") is not True:
                        raise ValueError("unexpected model request shape")
                    if self.headers.get("Authorization") != f"Bearer {KEY}":
                        raise ValueError("unexpected disposable credential")
                    with owner._lock:
                        if owner.requests:
                            raise ValueError("unexpected auxiliary model request")
                        owner.requests.append(payload)
                except Exception as error:  # keep response failure secret-free
                    self.fail(str(error))
                    return

                text = "protocol-ok"
                message = {
                    "id": "msg_host_protocol",
                    "type": "message",
                    "status": "completed",
                    "role": "assistant",
                    "content": [{"type": "output_text", "text": text, "annotations": []}],
                }
                response = {
                    "id": "resp_host_protocol",
                    "object": "response",
                    "created_at": int(time.time()),
                    "status": "completed",
                    "model": MODEL,
                    "output": [message],
                    "usage": {
                        "input_tokens": 1,
                        "output_tokens": 1,
                        "total_tokens": 2,
                        "input_tokens_details": {"cached_tokens": 0},
                        "output_tokens_details": {"reasoning_tokens": 0},
                    },
                }
                events = [
                    {"type": "response.created", "response": {**response, "status": "in_progress", "output": []}},
                    {"type": "response.output_item.added", "output_index": 0,
                     "item": {**message, "status": "in_progress", "content": []}},
                    {"type": "response.output_text.delta", "output_index": 0, "content_index": 0,
                     "item_id": message["id"], "delta": text},
                    {"type": "response.output_item.done", "output_index": 0, "item": message},
                    {"type": "response.completed", "response": response},
                ]
                body = b"".join(
                    f"event: {event['type']}\ndata: {json.dumps(event, separators=(',', ':'))}\n\n".encode()
                    for event in events
                )
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Connection", "close")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                self.close_connection = True

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever, name="fake-responses", daemon=True)
        thread.start()
        return self.server.server_port

    def stop(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()


class PlainWorkerTransport:
    """Map a dummy HTTPS tenant origin to one plain loopback DSH worker."""

    def __init__(self, origin, worker_port):
        self.origin = origin
        self.worker_port = worker_port

    def request(self, url, method="GET", body=None, headers=None, browser=None, edge=True):
        del edge
        parsed = urlsplit(url)
        if f"{parsed.scheme}://{parsed.netloc}" != self.origin:
            raise Failure("worker protocol request escaped disposable tenant origin")
        path = parsed.path or "/"
        if parsed.query:
            path += "?" + parsed.query
        outbound = dict(headers or {})
        for key in list(outbound):
            if key.lower() in {
                "origin", "x-forwarded-for", "x-forwarded-host", "x-forwarded-port",
                "x-forwarded-proto", "forwarded", "x-real-ip",
            }:
                outbound.pop(key, None)
        for key in list(outbound):
            if key.lower() == "host":
                outbound.pop(key, None)
        outbound["Host"] = f"127.0.0.1:{self.worker_port}"
        if browser is not None and browser.cookies and not any(k.lower() == "cookie" for k in outbound):
            outbound["Cookie"] = browser.header()
        connection = http.client.HTTPConnection("127.0.0.1", self.worker_port, timeout=15)
        try:
            connection.request(method, path, body=body, headers=outbound)
            response = connection.getresponse()
            response_headers = response.getheaders()
            if browser is not None:
                browser.accept(response_headers)
            data = response.read(MAX_BODY + 1)
            if len(data) > MAX_BODY:
                raise Failure("worker protocol response exceeded bound")
            return response.status, {key.lower(): value for key, value in response_headers}, data
        finally:
            connection.close()


class WorkerProcess:
    def __init__(self, process, lines, thread):
        self.process = process
        self.lines = lines
        self.thread = thread
        self.log = []

    @classmethod
    def start(cls, node, binary, home, dsh_home, cwd):
        env = {
            "PATH": f"{node.parent}{os.pathsep}/usr/bin:/bin",
            "HOME": str(home),
            "DSH_HOME": str(dsh_home),
            "LANG": "C.UTF-8",
            "NO_COLOR": "1",
            "TMPDIR": "/tmp",
        }
        process = subprocess.Popen(
            [str(node), str(binary), "web", "--no-open", "--host", "127.0.0.1", "--port", "0"],
            cwd=str(cwd), env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, bufsize=1, start_new_session=(os.name != "nt"),
        )
        lines = queue.Queue()

        def read_lines():
            assert process.stdout is not None
            for line in process.stdout:
                lines.put(line.rstrip("\r\n"))

        thread = threading.Thread(target=read_lines, name="dsh-output", daemon=True)
        thread.start()
        return cls(process, lines, thread)

    def wait_startup(self, timeout=30):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None and self.lines.empty():
                raise Failure("disposable DSH exited before startup")
            try:
                line = self.lines.get(timeout=0.25)
            except queue.Empty:
                continue
            self.log.append(line)
            match = STARTUP_RE.match(line)
            if match:
                return match.group(1)
        raise Failure("timed out waiting for disposable DSH startup")

    def stop(self):
        if self.process.poll() is None:
            try:
                if os.name != "nt":
                    os.killpg(self.process.pid, signal.SIGTERM)
                else:
                    self.process.terminate()
                self.process.wait(timeout=8)
            except (OSError, subprocess.TimeoutExpired):
                try:
                    if os.name != "nt":
                        os.killpg(self.process.pid, signal.SIGKILL)
                    else:
                        self.process.kill()
                except OSError:
                    pass
                try:
                    self.process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    pass
        if self.process.stdout is not None:
            self.process.stdout.close()


def exchange_startup(startup_url):
    parsed = urlsplit(startup_url)
    connection = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=10)
    try:
        path = parsed.path + ("?" + parsed.query if parsed.query else "")
        connection.request("GET", path, headers={"Host": parsed.netloc})
        response = connection.getresponse()
        if response.status != 303:
            raise Failure(f"worker token exchange returned {response.status}")
        header = response.getheader("Set-Cookie", "")
        cookie = header.split(";", 1)[0]
        if not cookie.startswith("dsh-auth-") or "=" not in cookie:
            raise Failure("worker token exchange did not issue dsh auth cookie")
        name, value = cookie.split("=", 1)
        response.read(4096)
        return Browser({name: value})
    finally:
        connection.close()


def write_fixture(root, fake_port):
    home = root / "home"
    dsh_home = home / ".dsh"
    cwd = root / "work"
    dsh_home.mkdir(parents=True, mode=0o700)
    cwd.mkdir(parents=True, mode=0o700)
    (dsh_home / ".credentials.yaml").write_text(
        "version: 1\nrefs:\n  AIGW_API_KEY: " + KEY + "\nrecords: {}\n", encoding="utf-8"
    )
    settings = {
        "agent-default-model": {"provider": "aigw", "model": MODEL},
        "llm-pi-ai": {"providers": {"aigw": {
            "apiKeyEnv": "AIGW_API_KEY",
            "api": "openai-responses",
            "baseURL": f"http://127.0.0.1:{fake_port}/v1",
            "models": [{"id": MODEL, "name": "Disposable host protocol"}],
            "defaultMaxTokens": 32,
            "retryPolicy": {"mode": "normal", "maxRetries": 0},
        }}},
    }
    settings_path = dsh_home / "settings.yaml"
    settings_path.write_text(json.dumps(settings), encoding="utf-8")
    # Leave title generation enabled: session/rename must actually suppress
    # its auxiliary request, as it will on the real host worker.
    for secret_path in (dsh_home / ".credentials.yaml", settings_path):
        os.chmod(secret_path, 0o600)
    return home, dsh_home, cwd


def scrub(text):
    return re.sub(r"\?token=[A-Za-z0-9_-]+", "?token=[REDACTED]", text).replace(KEY, "[dummy-key]")


def main():
    with tempfile.TemporaryDirectory(prefix="dshgw-host-protocol-") as temporary:
        root = Path(temporary)
        fake = FakeResponses()
        worker = None
        try:
            fake_port = fake.start()
            home, dsh_home, cwd = write_fixture(root, fake_port)
            worker = WorkerProcess.start(node_path, dsh_bin, home, dsh_home, cwd)
            startup_url = worker.wait_startup()
            startup = urlsplit(startup_url)
            if startup.port == 3080:
                raise Failure("disposable worker selected existing GUI port")
            browser = exchange_startup(startup_url)
            origin = f"https://tenant.invalid:{startup.port}"
            row = {
                "name": "protocol",
                "portal_url": origin,
                "public_port": startup.port,
                "worker_port": startup.port,
                "workspace": str(cwd),
            }
            transport = PlainWorkerTransport(origin, startup.port)
            result = prompt_worker(transport, row, browser, MODEL)
            if result.get("completed") is not True or result.get("has_reply") is not True:
                raise Failure("prompt_worker did not report a completed reply")
            if fake.errors:
                raise Failure("fake Responses backend rejected the worker request")
            if len(fake.requests) != 1:
                raise Failure("worker made an auxiliary model request")
            print("PASS dshgw host protocol: one completed turn, no auxiliary title request")
            return 0
        except (Failure, OSError, ValueError, AssertionError) as error:
            details = ""
            if worker is not None and worker.log:
                details = "\n" + "\n".join(scrub(line) for line in worker.log[-20:])
            print(f"FAIL: {error}{details}", file=sys.stderr)
            return 1
        finally:
            if worker is not None:
                worker.stop()
            fake.stop()


if __name__ == "__main__":
    raise SystemExit(main())
