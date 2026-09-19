#!/usr/bin/env python3
"""Drive isolated TLS fixtures; never contact Feishu or the running deployment.

The Go fixture reports actual received Fetch Metadata and cookie presence only.
Disposable Chrome profiles trust self-signed certificates ONLY for these local tests.
Requires Python websocket-client and an installed Chromium binary.
"""

import argparse
import contextlib
import json
import os
from pathlib import Path
import shutil
import signal
import ssl
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request

import websocket


class BrowserFailure(RuntimeError):
    pass


class CDP:
    def __init__(self, endpoint):
        self.socket = websocket.create_connection(endpoint, timeout=60, suppress_origin=True)
        self.serial = 0
        self.events = []

    def call(self, method, params=None, timeout=60):
        self.serial += 1
        serial = self.serial
        self.socket.send(json.dumps({'id': serial, 'method': method, 'params': params or {}}))
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                message = json.loads(self.socket.recv())
            except websocket.WebSocketTimeoutException:
                continue
            if 'method' in message:
                self.events.append(message['method'])
                continue
            if message.get('id') == serial:
                if 'error' in message:
                    raise BrowserFailure('CDP failed: ' + method)
                return message.get('result', {})
        raise BrowserFailure('CDP timed out: ' + method + '; events=' + ','.join(self.events[-12:]))

    def node(self, selector):
        root = self.call('DOM.getDocument', {'depth': 1})['root']['nodeId']
        return self.call('DOM.querySelector', {'nodeId': root, 'selector': selector}).get('nodeId', 0)

    def wait_node(self, selector):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            try:
                node = self.node(selector)
                if node:
                    return node
            except (BrowserFailure, KeyError):
                pass
            time.sleep(0.1)
        raise BrowserFailure('Expected fixture document did not load: ' + selector)

    def click(self, selector):
        node = self.wait_node(selector)
        model = self.call('DOM.getBoxModel', {'nodeId': node})['model']
        points = model['content']
        x, y = sum(points[0::2]) / 4, sum(points[1::2]) / 4
        self.call('Input.dispatchMouseEvent', {'type': 'mouseMoved', 'x': x, 'y': y})
        for kind in ('mousePressed', 'mouseReleased'):
            self.call('Input.dispatchMouseEvent', {'type': kind, 'x': x, 'y': y, 'button': 'left', 'clickCount': 1})


def stop_browser(process):
    if process is None:
        return
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=4)
    except subprocess.TimeoutExpired:
        pass
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGKILL)
    with contextlib.suppress(subprocess.TimeoutExpired):
        process.wait(timeout=2)


def probe_observations(callback):
    parsed = urllib.parse.urlsplit(callback)
    if parsed.scheme != 'https' or not parsed.port:
        raise BrowserFailure('Expected a private test callback origin')
    url = 'https://127.0.0.1:%d/_observations' % parsed.port
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPSHandler(context=ssl._create_unverified_context()),
    )
    with opener.open(url, timeout=5) as response:
        return json.load(response)


def launch(chrome, profile, log, host_rules):
    command = [
        chrome, '--headless', '--no-sandbox', '--disable-gpu',
        '--no-first-run', '--no-default-browser-check',
        '--disable-background-networking', '--window-size=1440,1000',
        '--ignore-certificate-errors',
        '--remote-debugging-port=0', '--user-data-dir=' + str(profile), 'about:blank',
    ]
    if host_rules:
        command.insert(6, '--host-resolver-rules=' + host_rules)
    return subprocess.Popen(command, stdout=log, stderr=log, start_new_session=True)


def connect(profile, timeout=60):
    active_port = profile / 'DevToolsActivePort'
    deadline = time.monotonic() + timeout
    while not active_port.exists():
        if time.monotonic() >= deadline:
            raise BrowserFailure('Chromium did not open DevTools')
        time.sleep(0.05)
    port = int(active_port.read_text().splitlines()[0])
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open('http://127.0.0.1:%d/json/list' % port, timeout=10) as response:
        page = next(row for row in json.load(response) if row['type'] == 'page')
    return CDP(page['webSocketDebuggerUrl'])


def scenario(chrome, args, mode, js_disabled):
    initial = len(probe_observations(args.callback_url))
    process = cdp = None
    with tempfile.TemporaryDirectory(prefix='feishu-handoff-browser-') as temporary:
        base = Path(temporary)
        profile = base / 'profile'
        profile.mkdir(mode=0o700)
        with (base / 'chrome.log').open('w', encoding='utf-8') as log:
            try:
                process = launch(chrome, profile, log, args.host_rules)
                cdp = connect(profile)
                cdp.call('Page.enable')
                cdp.call('DOM.enable')
                if js_disabled:
                    cdp.call('Emulation.setScriptExecutionDisabled', {'value': True})
                # Cold-start navigation in this environment can take tens of
                # seconds, so allow a generous deadline and one retry.
                navigation = cdp.call('Page.navigate', {'url': args.idp_url + '?mode=' + mode})
                if 'timeout' in navigation:
                    navigation = cdp.call('Page.navigate', {'url': args.idp_url + '?mode=' + mode})
                if navigation.get('errorText'):
                    raise BrowserFailure('Chromium could not navigate to the fixture IdP')
                # Commit a different-site document and physically click its link.
                cdp.click('#idp-continue')
                if js_disabled:
                    cdp.click('#feishu-continue')
                cdp.wait_node('#tenant-reached')
                rows = probe_observations(args.callback_url)[initial:]
                if [row['role'] for row in rows] != ['callback', 'portal', 'tenant']:
                    raise BrowserFailure('Expected exactly one complete three-hop chain')
                callback, portal, tenant = rows
                if callback['status'] != (200 if mode == 'handoff' else 303):
                    raise BrowserFailure('Callback response did not use the expected representation')
                if callback['site'] != 'cross-site':
                    raise BrowserFailure('IdP navigation did not reproduce cross-site metadata')
                for row in rows:
                    if row['origin'] or row['mode'] != 'navigate' or row['dest'] != 'document':
                        raise BrowserFailure('Unexpected top-level navigation metadata')
                expected_site = 'same-site' if mode == 'handoff' else 'cross-site'
                if portal['site'] != expected_site or tenant['site'] != expected_site:
                    raise BrowserFailure('Unexpected site classification after callback')
                if portal['status'] != 303 or tenant['status'] != 200:
                    raise BrowserFailure('Portal-to-tenant fixture chain failed')
                if not portal['ticketCookie'] or not tenant['ticketCookie']:
                    raise BrowserFailure('Ticket cookie did not reach both same-host ports')
                print(json.dumps({'mode': mode, 'jsDisabled': js_disabled, 'requests': rows}, ensure_ascii=False))
            except Exception:
                print(json.dumps({'mode': mode, 'jsDisabled': js_disabled, 'observed': probe_observations(args.callback_url)[initial:]}))
                raise
            finally:
                if cdp:
                    with contextlib.suppress(Exception):
                        cdp.socket.close()
                stop_browser(process)


def interrupted(signum, frame):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('idp', 'callback', 'portal', 'tenant'):
        parser.add_argument('--' + name + '-url', required=True)
    parser.add_argument('--chrome', default=os.environ.get('CHROME_BIN', ''))
    parser.add_argument('--host-rules', default='')
    args = parser.parse_args()
    candidates = [args.chrome, shutil.which('chromium'), shutil.which('google-chrome'),
                  '/home/winger/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome',
                  '/home/winger/.cache/ms-playwright/chromium-1187/chrome-linux/chrome']
    chrome = next((c for c in candidates if c and os.path.isfile(c) and os.access(c, os.X_OK)), None)
    if not chrome:
        raise BrowserFailure('Chromium is unavailable')
    signal.signal(signal.SIGTERM, interrupted)
    scenario(chrome, args, 'legacy', False)
    scenario(chrome, args, 'handoff', False)
    scenario(chrome, args, 'handoff', True)
    print('PASS actual TLS browser redirect metadata, unchanged cookies, automatic and no-JS handoffs')


if __name__ == '__main__':
    try:
        main()
    except (BrowserFailure, KeyboardInterrupt) as error:
        print('FAIL:', str(error) or 'interrupted')
        raise SystemExit(1)
