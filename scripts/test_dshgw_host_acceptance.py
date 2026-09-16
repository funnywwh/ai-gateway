"""Unprivileged orchestration/unit tests; these are NOT host acceptance."""
import copy
from contextlib import ExitStack, redirect_stdout
import io
import json
import os
import socket
import http.client
from pathlib import Path
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from urllib.parse import parse_qs, urlsplit
from unittest.mock import patch

import dshgw_host_acceptance as host


def row(name, base, uid=1001, prefix="sk-aaaaaaaaa"):
    return {"name": name, "uid": uid, "user": "dsh-" + name, "unit": "dsh-worker@" + name + ".service",
            "public_port": 32601 if uid == 1001 else 32602, "worker_port": 32100 if uid == 1001 else 32101,
            "created_at": "2026-01-01T00:00:00Z", "workspace": str(base / "workspaces" / name),
            "dsh_home": str(base / "tenants" / name / ".dsh"), "key_prefix": prefix, "previous_prefixes": [],
            "handshake_path": str(base / "handshake" / (name + ".url")),
            "gateway_key_path": str(base / "config" / name / "gateway.key"),
            "aigw_base_url": "http://aigw.test:8088", "key_revalidate": "off", "worker_slice": "dsh-workers.slice",
            "portal_url": "https://dsh.test:32600/"}


def event(seq, kind, data):
    return {"type": "event", "event": {"seq": seq, "type": kind, "data": data}}


def completed(request="prompt", turn=1):
    return [event(0, "turn/start", {"turn": turn}),
            event(1, "user/message", {"source": {"rpcId": request}}),
            event(2, "assistant/message", {"turn": turn, "message": {"content": [{"type": "text", "text": "OK"}]}}),
            event(3, "turn/end", {"turn": turn, "reason": {"kind": "completed"}})]


class FakeCLI:
    def __init__(self, config, base, rows=None, fail_create=None):
        self.binary, self.config, self.base = "/opt/dshgw/bin/dshgw", str(config), base
        self.current, self.calls = copy.deepcopy(rows or []), []
        self.fail_create = fail_create

    def rows(self):
        return copy.deepcopy(self.current)

    def call(self, *args, allowed=(0,)):
        self.calls.append(args)
        output = b""
        if args == ("login-url",):
            output = b"https://dsh.test:32600/\n"
        elif args[:2] == ("tenant", "create"):
            if self.fail_create and args[-1].endswith(self.fail_create):
                raise host.Failure("injected create failure")
            self.current.append(row(args[-1], self.base))
        elif args[:2] == ("tenant", "restart"):
            self.on_restart(args[-1])
        elif args[:2] == ("tenant", "remove"):
            self.current = [value for value in self.current if value["name"] != args[-1]]
            output = ("removed tenant; snapshot: /private/snapshot-" + args[-1] + ".tar.gz\n").encode()
        return subprocess.CompletedProcess(args, 0, output, b"")


class FakeHTTP:
    def __init__(self, models=("deepseek-flash",)):
        self.calls, self.models = [], models

    def request(self, url, *args, **kwargs):
        self.calls.append((url, args, kwargs))
        if url.endswith("/v1/models"):
            return 200, {}, json.dumps({"data": [{"id": model} for model in self.models]}).encode()
        return 200, {}, b"<html></html>"


class FakeResumeHTTP:
    """Exercise all shared verification branches without network/model calls."""
    def __init__(self, rows, keys):
        self.rows, self.keys, self.calls = rows, keys, []
        self.tokens, self.logins, self.websocket_closes = set(), 0, []
        self.portal = host.origin_only(rows[0]["portal_url"])

    def request(self, url, method="GET", body=None, headers=None, browser=None, edge=True):
        self.calls.append((url, method))
        parsed = urlsplit(url)
        origin, path = f"{parsed.scheme}://{parsed.netloc}", parsed.path or "/"
        if path == "/v1/models":
            return 200, {}, b'{"data":[{"id":"deepseek-flash"}]}'
        if origin == self.portal:
            if path == "/login":
                key = parse_qs(body.decode())["key"][0]
                tenant = self.rows[self.keys.index(key)]
                self.logins += 1
                token = "fake-browser-token-" + str(self.logins)
                name = "dshgw_s_" + tenant["name"]
                self.tokens.add((name, token))
                browser.accept([("Set-Cookie", f"{name}={token}; Path=/; Secure; HttpOnly; SameSite=Lax")])
                return 302, {"location": host.tenant_origin(tenant) + "/"}, b""
            if path == "/logout":
                self.tokens.difference_update(browser.cookies.items())
                browser.cookies.clear()
                return 303, {"location": "/"}, b""
            return 200, {}, b"portal"
        tenant = next(row for row in self.rows if host.tenant_origin(row) == origin)
        if (headers or {}).get("Origin", origin) != origin:
            return 403, {}, b""
        name = "dshgw_s_" + tenant["name"]
        if browser is None or (name, browser.cookies.get(name)) not in self.tokens:
            return 302, {"location": self.portal + "/"}, b""
        if path == "/browser-fs/ws":
            return 426, {}, b""
        if path == "/plugins/":
            return 200, {}, b"client module"
        if path == "/api/session/create":
            envelope = json.loads(body)
            return 200, {}, json.dumps({"type": "server-response", "rpcId": envelope["rpcId"],
                "result": {"ok": True, "value": {"sessionId": "after-restart"}}}).encode()
        if path == "/":
            return 200, {}, b'<script src="/plugins/??dsh-browser-fs/client.js&amp;rev=fake"></script>'
        raise AssertionError("unexpected fake HTTP request")

    def websocket(self, origin, browser, path):
        assert path == "/browser-fs/ws"
        return SimpleNamespace(close=lambda: self.websocket_closes.append(origin))


class FakeUnits:
    def __init__(self, rows, overrides=None, fail_start=None):
        self.rows = {row["unit"]: row for row in rows}
        self.calls, self.fail_start = [], fail_start
        self.props = {unit: {"Id": unit, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead",
                             "User": row["user"], "MainPID": "0"} for unit, row in self.rows.items()}
        for unit, props in (overrides or {}).items():
            self.props[unit].update(props)

    def __call__(self, argv, allowed=(0,), timeout=180):
        self.calls.append(tuple(argv))
        assert argv[0] == "systemctl" and argv[-1] in self.rows, "attempted to operate on an unrecorded unit"
        verb, unit = argv[1], argv[-1]
        if verb == "show":
            return subprocess.CompletedProcess(argv, 0, "".join(f"{key}={value}\n" for key, value in self.props[unit].items()).encode(), b"")
        if verb == "start":
            self.props[unit].update(ActiveState="active", SubState="running", MainPID="111")
            if unit == self.fail_start:
                raise host.Failure("injected partially-completed worker start")
        elif verb == "stop":
            self.props[unit].update(ActiveState="inactive", SubState="dead", MainPID="0")
        else:
            raise AssertionError("unexpected fake systemctl operation")
        return subprocess.CompletedProcess(argv, 0, b"", b"")

    def restart(self, name):
        row = next(row for row in self.rows.values() if row["name"] == name)
        props = self.props[row["unit"]]
        props["MainPID"] = str(int(props["MainPID"]) + 1)
        Path(row["handshake_path"]).write_text("changed disposable handshake")

    def properties(self, row):
        assert self.props[row["unit"]]["ActiveState"] == "active"
        return {"pid": int(self.props[row["unit"]]["MainPID"])}


class AcceptanceTests(unittest.TestCase):
    def setUp(self):
        # Fake orchestration must not print real-host-looking PASS attestations
        # in make output; unittest's own result still goes to stderr.
        output = redirect_stdout(io.StringIO())
        output.__enter__()
        self.addCleanup(output.__exit__, None, None, None)
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.run = self.base / "run"
        self.run.mkdir(mode=0o700)
        self.config = self.base / "dshgw.yaml"
        self.config.write_text("public_host: dsh.test\n")
        self.keys = ["sk-aaaaaaaaa-secret-a", "sk-bbbbbbbbb-secret-b"]
        self.key_files = []
        for name, key in zip(("a", "b"), self.keys):
            path = self.base / (name + ".key")
            path.write_text(key + "\n")
            path.chmod(0o600)
            self.key_files.append(str(path))

    def state(self, cli, rows, before=None):
        engine = host.Acceptance(cli, self.run)
        engine.state = {"schema": 1, "binary": cli.binary, "config": cli.config, "run_id": "aabbccdd",
                        "config_sha256": host.fingerprint(self.config.read_bytes()), "attempted": [value["name"] for value in rows],
                        "tenants": copy.deepcopy(rows), "before": [host.identity(value) for value in (before or [])],
                        "keys": [], "cookies": {"dshgw_s_test": "private-browser-token"}}
        engine.save()
        return engine

    def resume_fixture(self, overrides=None, fail_start=None):
        rows = [row("m51-e2e-aabbccdd-a", self.base),
                row("m51-e2e-aabbccdd-b", self.base, uid=1002, prefix="sk-bbbbbbbbb")]
        existing = row("existing", self.base, uid=1003, prefix="sk-zzzzzzzzz")
        cli = FakeCLI(self.config, self.base, [*rows, existing])
        engine = self.state(cli, rows, before=[existing])
        engine.state.update(model="deepseek-flash", keys=[
            {"path": path, "sha256": host.fingerprint(key.encode())} for path, key in zip(self.key_files, self.keys)])
        engine.report.update(phase="baseline", status="failed", next="original diagnosis instructions",
                             portal_url=rows[0]["portal_url"], tenants=[{"name": row["name"], "url": host.tenant_origin(row) + "/"} for row in rows],
                             checks=[{"check": "original-isolation-check", "status": "passed", "detail": {"kept": True}}],
                             stop_warnings=["original warning"], diagnostic={"untouched": True})
        engine.save()
        engine.transport = FakeResumeHTTP(rows, self.keys)
        for tenant in rows:
            handshake = Path(tenant["handshake_path"])
            handshake.parent.mkdir(exist_ok=True)
            handshake.write_text("original disposable handshake")
        units = FakeUnits(rows, overrides, fail_start)
        cli.on_restart = units.restart
        return engine, cli, rows, units

    def resume_mocks(self, rows, units):
        stack = ExitStack()
        stack.enter_context(patch.object(host, "run_command", side_effect=units))
        stack.enter_context(patch.object(host.pwd, "getpwnam", side_effect=lambda name: SimpleNamespace(
            pw_uid=next(row["uid"] for row in rows if row["user"] == name))))
        stack.enter_context(patch.object(host, "file_checks"))
        stack.enter_context(patch.object(host, "unit_properties", side_effect=units.properties))
        return stack

    def test_websocket_close_closes_response_before_owned_stream(self):
        client, server = socket.socketpair()
        self.addCleanup(client.close)
        self.addCleanup(server.close)
        server.sendall(b"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
        response = http.client.HTTPResponse(client)
        response.begin()
        probe = host.MuxSocket.__new__(host.MuxSocket)
        probe.sock, probe.response = client, response
        probe.close()
        self.assertTrue(response.closed)
        self.assertIsNone(response.fp)
        self.assertEqual(client.fileno(), -1)
        probe.close()  # idempotent, including explicit caller + error cleanup
        response.close()  # its finalizer must not flush an already-closed fp

    def test_secret_files_require_0600_and_no_symlinks(self):
        self.assertEqual(host.read_key(self.key_files[0]), self.keys[0])
        path = Path(self.key_files[0])
        path.chmod(0o640)
        with self.assertRaises(host.Failure) as captured:
            host.read_key(path)
        self.assertNotIn(self.keys[0], str(captured.exception))
        path.chmod(0o600)
        link = self.base / "link.key"
        link.symlink_to(path)
        with self.assertRaises(host.Failure):
            host.read_key(link)

    def test_private_state_and_public_report_are_separate(self):
        engine = self.state(FakeCLI(self.config, self.base), [])
        public = (self.run / "report.json").read_text()
        private = (self.run / "state.json").read_text()
        self.assertNotIn("private-browser-token", public)
        self.assertIn("private-browser-token", private)
        self.assertEqual((self.run / "state.json").stat().st_mode & 0o777, 0o600)
        self.assertFalse(engine.report["overall_complete"])

    def test_failed_cli_output_is_private_not_in_public_error(self):
        secret = "Bearer sk-never-share-this"
        def runner(argv, allowed=(0,), timeout=180):
            return subprocess.CompletedProcess(argv, 1, b"", secret.encode())
        cli = host.CLI("/opt/dshgw/bin/dshgw", str(self.config), runner)
        cli.audit_dir = self.run
        with self.assertRaises(host.Failure) as captured:
            cli.call("tenant", "remove", "--purge", "--yes", "m51-e2e-aabbccdd-a")
        self.assertNotIn(secret, str(captured.exception))
        log = self.run / "cli.log"
        self.assertIn(secret, log.read_text())
        self.assertEqual(log.stat().st_mode & 0o777, 0o600)

    def test_existing_run_directory_is_never_reused(self):
        with self.assertRaises(FileExistsError):
            host.private_dir(self.run, create=True)

    def test_baseline_refuses_bound_keys_before_create(self):
        existing = row("existing", self.base)
        cli = FakeCLI(self.config, self.base, [existing])
        engine = host.Acceptance(cli, self.run, FakeHTTP())
        with self.assertRaises(host.Failure):
            engine.baseline(self.key_files, "deepseek-flash", "http://aigw.test:8088")
        self.assertFalse(any(call[:2] == ("tenant", "create") for call in cli.calls))

    def test_missing_model_fails_before_creating_tenants(self):
        cli = FakeCLI(self.config, self.base)
        engine = host.Acceptance(cli, self.run, FakeHTTP(models=()))
        with self.assertRaises(host.Failure):
            engine.baseline(self.key_files, "deepseek-flash", "http://aigw.test:8088")
        self.assertFalse(any(call[:2] == ("tenant", "create") for call in cli.calls))

    def test_failed_second_create_only_stops_recorded_test_unit(self):
        cli = FakeCLI(self.config, self.base, fail_create="-b")
        engine = host.Acceptance(cli, self.run, FakeHTTP())
        with patch.object(host, "run_command") as commands:
            with self.assertRaises(host.Failure):
                engine.baseline(self.key_files, "deepseek-flash", "http://aigw.test:8088")
        self.assertEqual(len(engine.state["tenants"]), 1)
        self.assertEqual(commands.call_args.args[0], ["systemctl", "stop", engine.state["tenants"][0]["unit"]])
        self.assertFalse(any(call[:2] == ("tenant", "remove") for call in cli.calls))
        report = (self.run / "report.json").read_text()
        self.assertEqual(json.loads(report)["status"], "failed")
        for key in self.keys:
            self.assertNotIn(key, report)

    def test_cleanup_refuses_uid_or_configuration_drift(self):
        saved = row("m51-e2e-aabbccdd-a", self.base)
        changed = copy.deepcopy(saved)
        changed["uid"] = 1002
        cli = FakeCLI(self.config, self.base, [changed])
        engine = self.state(cli, [saved])
        with self.assertRaises(host.Failure):
            engine.cleanup()
        self.assertFalse(cli.calls)
        cli.current = [saved]
        self.config.write_text("public_host: another.test\n")
        with self.assertRaises(host.Failure):
            engine.cleanup()
        self.assertFalse(cli.calls)

    def test_cleanup_uses_snapshot_first_cli_only_for_owned_tenants(self):
        target = row("m51-e2e-aabbccdd-a", self.base)
        existing = row("existing", self.base, uid=1002, prefix="sk-zzzzzzzzz")
        cli = FakeCLI(self.config, self.base, [target, existing])
        engine = self.state(cli, [target], before=[existing])
        engine.cleanup()
        self.assertEqual(cli.calls, [("tenant", "remove", "--purge", "--yes", target["name"])])
        self.assertEqual([value["name"] for value in cli.current], ["existing"])
        self.assertTrue(engine.report["snapshots"][0]["path"].endswith(".tar.gz"))
        self.assertEqual(engine.state["cookies"], {})
        self.assertFalse(engine.report["overall_complete"])

    def test_unrecorded_create_attempt_cannot_be_claimed_clean(self):
        cli = FakeCLI(self.config, self.base)
        engine = self.state(cli, [])
        engine.state["attempted"] = ["m51-e2e-aabbccdd-a"]
        engine.save()
        with self.assertRaises(host.Failure):
            engine.cleanup()
        self.assertFalse(cli.calls)

    def test_browser_rejects_worker_cookie_and_unsafe_gateway_cookie(self):
        for value in ("dsh-auth-secret=value", "dshgw_s_alice=value; Path=/"):
            with self.assertRaises(host.Failure):
                host.Browser().accept([("Set-Cookie", value)])
        browser = host.Browser()
        browser.accept([("Set-Cookie", "dshgw_s_alice=value; Path=/; Secure; HttpOnly; SameSite=Lax")])
        self.assertEqual(browser.header(), "dshgw_s_alice=value")
        browser.accept([("Set-Cookie", "dshgw_s_alice=; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=0")])
        self.assertEqual(browser.cookies, {})

    def test_completed_turn_requires_own_prompt_and_same_turn(self):
        self.assertTrue(host.completed_turn(completed(), "prompt"))
        self.assertFalse(host.completed_turn(completed("another"), "prompt"))
        wrong = completed()
        wrong[2]["event"]["data"]["turn"] = 2
        with self.assertRaises(host.Failure):
            host.completed_turn(wrong, "prompt")
        with self.assertRaises(host.Failure):
            host.completed_turn(completed()[1:], "prompt")
        error = completed()
        error[-1]["event"]["data"]["reason"] = {"kind": "error", "error": {"message": "private detail"}}
        with self.assertRaises(host.Failure) as captured:
            host.completed_turn(error, "prompt")
        self.assertNotIn("private detail", str(captured.exception))

    def test_resume_requires_both_explicit_confirmations_before_loading(self):
        engine, cli, _, _ = self.resume_fixture()
        for flags in ({}, {"confirm_start_workers": True}, {"allow_model_call": True}):
            with self.subTest(flags=flags), patch.object(engine, "load") as load, patch.object(host, "run_command") as commands:
                with self.assertRaises(host.Failure):
                    engine.resume_baseline(**flags)
                load.assert_not_called()
                commands.assert_not_called()
        self.assertFalse(cli.calls)

    def test_resume_fingerprint_drift_runs_no_commands_and_preserves_evidence(self):
        for changed in ("config", "key"):
            with self.subTest(changed=changed):
                engine, cli, _, _ = self.resume_fixture()
                path = self.config if changed == "config" else Path(self.key_files[0])
                original = path.read_bytes()
                state, report = (self.run / "state.json").read_bytes(), (self.run / "report.json").read_bytes()
                path.write_text("changed-config" if changed == "config" else self.keys[0] + "-different")
                try:
                    with patch.object(cli, "rows", wraps=cli.rows) as listing, patch.object(host, "run_command") as commands:
                        with self.assertRaises(host.Failure):
                            engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
                        listing.assert_not_called()
                        commands.assert_not_called()
                    self.assertFalse(cli.calls)
                    self.assertEqual((self.run / "state.json").read_bytes(), state)
                    self.assertEqual((self.run / "report.json").read_bytes(), report)
                finally:
                    path.write_bytes(original)

    def test_resume_refuses_unknown_names_before_commands(self):
        engine, cli, rows, _ = self.resume_fixture()
        engine.state["attempted"].append("existing")
        engine.save()
        with patch.object(cli, "rows", wraps=cli.rows) as listing, patch.object(host, "run_command") as commands:
            with self.assertRaises(host.Failure):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
            listing.assert_not_called()
            commands.assert_not_called()
        self.assertFalse(cli.calls)
        engine.state["attempted"] = [row["name"] for row in rows]
        engine.state["tenants"][1] = row("m51-e2e-aabbccdd-c", self.base, uid=1002, prefix="sk-bbbbbbbbb")
        engine.save()
        with patch.object(host, "run_command") as commands, self.assertRaises(host.Failure):
            engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        commands.assert_not_called()

    def test_resume_requires_two_current_matching_tenants_and_no_unrecorded_run_tenant(self):
        for changed in ("missing", "uid", "extra", "duplicate-saved"):
            with self.subTest(changed=changed):
                engine, cli, rows, _ = self.resume_fixture()
                if changed == "missing":
                    cli.current = cli.current[1:]
                elif changed == "uid":
                    cli.current[0]["uid"] = 9999
                elif changed == "extra":
                    cli.current.append(row("m51-e2e-aabbccdd-c", self.base, uid=1004, prefix="sk-yyyyyyyyy"))
                else:
                    engine.state["tenants"][1] = copy.deepcopy(rows[0])
                    engine.save()
                with patch.object(host, "run_command") as commands, self.assertRaises(host.Failure):
                    engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
                commands.assert_not_called()
                self.assertFalse(cli.calls)

    def test_resume_refuses_revoked_cleaned_or_passed_run_and_missing_model(self):
        for phase, status, model in (("revoked", "passed", "deepseek-flash"), ("cleanup", "passed", "deepseek-flash"),
                                      ("baseline", "passed", "deepseek-flash"), ("baseline", "failed", None)):
            with self.subTest(phase=phase, status=status, model=model):
                engine, cli, _, _ = self.resume_fixture()
                engine.report.update(phase=phase, status=status)
                engine.state["model"] = model
                engine.save()
                with patch.object(host, "run_command") as commands, self.assertRaises(host.Failure):
                    engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
                commands.assert_not_called()
                self.assertFalse(cli.calls)

    def test_resume_rejects_transitional_unknown_or_mismatched_units_before_start(self):
        bad_properties = ({"ActiveState": "activating", "SubState": "start"}, {"ActiveState": "deactivating"},
                          {"ActiveState": "failed"}, {"LoadState": "not-found"}, {"MainPID": "123"},
                          {"User": "root"}, {"Id": "unrecorded.service"}, {"SubState": "unknown"})
        for properties in bad_properties:
            with self.subTest(properties=properties):
                engine, cli, rows, units = self.resume_fixture()
                units.props[rows[1]["unit"]].update(properties)
                report = (self.run / "report.json").read_bytes()
                with self.resume_mocks(rows, units), patch.object(host, "prompt_worker") as prompt:
                    with self.assertRaises(host.Failure):
                        engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
                    prompt.assert_not_called()
                self.assertEqual([call[1] for call in units.calls], ["show", "show"])
                self.assertEqual((self.run / "report.json").read_bytes(), report)
                self.assertFalse(cli.calls)

    def test_resume_rejects_changed_os_uid_before_start(self):
        engine, cli, _, _ = self.resume_fixture()
        with patch.object(host.pwd, "getpwnam", return_value=SimpleNamespace(pw_uid=9999)), patch.object(host, "run_command") as commands:
            with self.assertRaises(host.Failure):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
            commands.assert_not_called()
        self.assertFalse(cli.calls)

    def test_resume_reruns_verification_without_create_or_purge_and_keeps_prior_report(self):
        engine, cli, rows, units = self.resume_fixture()
        prior = copy.deepcopy(engine.report)
        original_state = copy.deepcopy(engine.state)
        with self.resume_mocks(rows, units), patch.object(host, "prompt_worker", return_value={"completed": True}) as prompt:
            engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        self.assertEqual([call[1] for call in units.calls], ["show", "show", "start", "start"])
        self.assertEqual([call[-1] for call in units.calls[2:]], [row["unit"] for row in rows])
        self.assertEqual(cli.calls, [("tenant", "restart", rows[0]["name"])])
        self.assertEqual(prompt.call_count, 2)
        for call, tenant in zip(prompt.call_args_list, rows):
            self.assertEqual(call.args[1], tenant)
            self.assertEqual(call.args[3], original_state["model"])
        self.assertEqual(len(engine.transport.websocket_closes), 2)
        self.assertEqual(engine.report["checks"][:len(prior["checks"])], prior["checks"])
        self.assertEqual(engine.report["stop_warnings"], prior["stop_warnings"])
        self.assertEqual(engine.report["diagnostic"], prior["diagnostic"])
        self.assertEqual(engine.report["tenants"], prior["tenants"])
        self.assertEqual(engine.report["pending"], prior["pending"])
        attempt = engine.report["resume_attempts"][-1]
        self.assertEqual(attempt["previous_status"], "failed")
        self.assertEqual(attempt["previous_next"], prior["next"])
        self.assertEqual(attempt["status"], "passed")
        self.assertEqual(attempt["check_offset"], len(prior["checks"]))
        self.assertEqual(engine.report["status"], "passed")
        self.assertEqual(engine.report["phase"], "resume-baseline")
        self.assertFalse(engine.report["overall_complete"])
        for field in ("tenants", "before", "attempted", "keys", "model", "config_sha256"):
            self.assertEqual(engine.state[field], original_state[field])
        for secret in [*self.keys, "fake-browser-token", "private-browser-token"]:
            self.assertNotIn(secret, (self.run / "report.json").read_text())
        self.assertTrue(all(props["ActiveState"] == "active" for props in units.props.values()))

    def test_resume_does_not_start_an_already_active_recorded_unit(self):
        engine, cli, rows, units = self.resume_fixture()
        units.props[rows[0]["unit"]].update(ActiveState="active", SubState="running", MainPID="321")
        with self.resume_mocks(rows, units), patch.object(host, "prompt_worker", return_value={"completed": True}):
            engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        self.assertEqual([call for call in units.calls if call[1] == "start"], [("systemctl", "start", rows[1]["unit"])])
        self.assertEqual(cli.calls, [("tenant", "restart", rows[0]["name"])])

    def test_resume_partial_start_failure_stops_both_matching_units_and_keeps_evidence(self):
        engine, cli, rows, units = self.resume_fixture(fail_start="dsh-worker@m51-e2e-aabbccdd-b.service")
        prior = copy.deepcopy(engine.report)
        with self.resume_mocks(rows, units), patch.object(host, "prompt_worker") as prompt:
            with self.assertRaises(host.Failure):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
            prompt.assert_not_called()
        self.assertEqual([call[1] for call in units.calls], ["show", "show", "start", "start", "stop", "stop"])
        self.assertEqual([call[-1] for call in units.calls[-2:]], [row["unit"] for row in rows])
        self.assertFalse(cli.calls)
        self.assertEqual(engine.report["checks"], prior["checks"])
        self.assertEqual(engine.report["stop_warnings"], prior["stop_warnings"])
        attempt = engine.report["resume_attempts"][-1]
        self.assertEqual(attempt["status"], "failed")
        self.assertEqual(attempt["start_attempted"], [row["unit"] for row in rows])
        self.assertEqual(attempt["started_units"], [rows[0]["unit"]])
        self.assertTrue(all(props["ActiveState"] == "inactive" for props in units.props.values()))
        self.assertEqual([row["name"] for row in cli.current], [rows[0]["name"], rows[1]["name"], "existing"])

    def test_resume_model_failure_stops_workers_and_retains_accumulated_checks(self):
        engine, cli, rows, units = self.resume_fixture()
        with self.resume_mocks(rows, units), patch.object(host, "prompt_worker", side_effect=host.Failure("model failed")):
            with self.assertRaises(host.Failure):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        self.assertEqual([call[-1] for call in units.calls if call[1] == "stop"], [row["unit"] for row in rows])
        self.assertGreater(len(engine.report["checks"]), 1)
        self.assertEqual(engine.report["checks"][0]["check"], "original-isolation-check")
        self.assertEqual(engine.report["resume_attempts"][-1]["status"], "failed")
        self.assertFalse(cli.calls)

    def test_resume_failure_never_stops_changed_identity(self):
        engine, cli, rows, units = self.resume_fixture()
        def fail_and_change(*args):
            cli.current[0]["uid"] = 9999
            raise host.Failure("injected failure with concurrent identity change")
        with self.resume_mocks(rows, units), patch.object(host, "prompt_worker", side_effect=fail_and_change):
            with self.assertRaises(host.Failure):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        self.assertEqual([call[-1] for call in units.calls if call[1] == "stop"], [rows[1]["unit"]])
        self.assertIn(rows[0]["name"], engine.report["stop_warnings"][-1])
        self.assertEqual(engine.report["stop_warnings"][0], "original warning")
        self.assertFalse(cli.calls)

    def test_resume_after_failed_resume_appends_attempts_without_erasing_evidence(self):
        engine, cli, rows, units = self.resume_fixture(fail_start="dsh-worker@m51-e2e-aabbccdd-b.service")
        with self.resume_mocks(rows, units), self.assertRaises(host.Failure):
            engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        prior = copy.deepcopy(engine.report)
        units.fail_start = None
        with self.resume_mocks(rows, units), patch.object(host, "prompt_worker", return_value={"completed": True}):
            engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        self.assertEqual(len(engine.report["resume_attempts"]), 2)
        self.assertEqual(engine.report["resume_attempts"][0], prior["resume_attempts"][0])
        self.assertEqual(engine.report["resume_attempts"][1]["previous_phase"], "resume-baseline")
        self.assertEqual(engine.report["resume_attempts"][1]["status"], "passed")
        self.assertEqual(engine.report["checks"][:len(prior["checks"])], prior["checks"])
        self.assertEqual(engine.report["stop_warnings"], prior["stop_warnings"])
        self.assertEqual(cli.calls, [("tenant", "restart", rows[0]["name"])])

    def test_resume_interrupt_during_start_stops_matching_workers(self):
        engine, cli, rows, units = self.resume_fixture()
        def interrupt(argv, **kwargs):
            result = units(argv, **kwargs)
            if argv[1] == "start":
                raise KeyboardInterrupt()
            return result
        with self.resume_mocks(rows, units), patch.object(host, "run_command", side_effect=interrupt), patch.object(host, "prompt_worker") as prompt:
            with self.assertRaises(KeyboardInterrupt):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
            prompt.assert_not_called()
        self.assertEqual([call[1] for call in units.calls], ["show", "show", "start", "stop", "stop"])
        self.assertEqual(engine.report["resume_attempts"][-1]["status"], "failed")
        self.assertTrue(all(props["ActiveState"] == "inactive" for props in units.props.values()))
        self.assertFalse(cli.calls)

    def test_resume_cleanup_stop_failure_does_not_skip_other_worker(self):
        engine, cli, rows, units = self.resume_fixture()
        def commands(argv, **kwargs):
            if argv[1] == "stop" and argv[-1] == rows[0]["unit"]:
                units.calls.append(tuple(argv))
                raise host.Failure("injected stop failure")
            return units(argv, **kwargs)
        with self.resume_mocks(rows, units), patch.object(host, "run_command", side_effect=commands), patch.object(host, "prompt_worker", side_effect=host.Failure("model failed")):
            with self.assertRaises(host.Failure):
                engine.resume_baseline(confirm_start_workers=True, allow_model_call=True)
        self.assertEqual([call[-1] for call in units.calls if call[1] == "stop"], [row["unit"] for row in rows])
        self.assertEqual(engine.report["stop_warnings"][0], "original warning")
        self.assertIn(rows[0]["name"], engine.report["stop_warnings"][-1])
        self.assertEqual(units.props[rows[1]["unit"]]["ActiveState"], "inactive")
        self.assertFalse(cli.calls)

    def test_resume_cli_requires_flags_and_refuses_key_model_endpoint_overrides(self):
        argv = ["resume-baseline", "--run-dir", str(self.run)]
        for extra in ([], ["--confirm-start-workers"], ["--allow-model-call"],
                      ["--confirm-start-workers", "--allow-model-call", "--model", "different"],
                      ["--confirm-start-workers", "--allow-model-call", "--key-a", self.key_files[0]],
                      ["--confirm-start-workers", "--allow-model-call", "--key-b", self.key_files[1]],
                      ["--confirm-start-workers", "--allow-model-call", "--aigw-url", "http://different.test"]):
            with self.subTest(extra=extra), patch.object(host.os, "geteuid", return_value=0), patch.object(host, "Acceptance") as acceptance, patch.object(host, "private_dir") as private, patch("sys.stderr", new=io.StringIO()):
                self.assertEqual(host.main(argv + extra), 1)
                acceptance.assert_not_called()
                private.assert_not_called()
        with patch.object(host.os, "geteuid", return_value=0), patch.object(host, "private_dir"), patch.object(host, "Acceptance") as acceptance, patch("sys.stdout", new=io.StringIO()):
            self.assertEqual(host.main(argv + ["--confirm-start-workers", "--allow-model-call"]), 0)
            acceptance.return_value.resume_baseline.assert_called_once_with(confirm_start_workers=True, allow_model_call=True)
            acceptance.return_value.baseline.assert_not_called()
            acceptance.return_value.cleanup.assert_not_called()

    def test_external_probe_requires_real_https_and_no_credentials(self):
        for url in ("http://dsh.test:32600/", "https://user:secret@dsh.test:32600/", "https://dsh.test:32600/path"):
            with self.assertRaises(host.Failure):
                host.origin_only(url)
        report = self.base / "public-report.json"
        report.write_text(json.dumps({"schema": 1, "portal_url": "https://dsh.test:32600/", "tenants": [
            {"url": "https://dsh.test:32601/"}, {"url": "https://dsh.test:32602/"}]}))
        class ExternalHTTP:
            def request(self, url):
                return (200, {}, b"") if ":32600/" in url else (302, {"location": "https://dsh.test:32600/"}, b"")
        result = host.external_probe(report, ExternalHTTP())
        self.assertEqual(len(result["checks"]), 3)
        self.assertFalse(result["overall_complete"])


if __name__ == "__main__":
    unittest.main()
