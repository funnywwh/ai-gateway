#!/usr/bin/env python3
"""Real DSH browser-workspace UI registration smoke test (Linux/POSIX).

Dependencies: Python 3, websocket-client (``python3 -m pip install
websocket-client``), Node >=22, an already installed DSH runtime, and Chromium
with its system libraries. No npm install/build or gateway backend is needed.

Usage::

    python3 scripts/browser_workspace_ui_smoke.py \
        --node /home/winger/.local/node-v22.23.1-linux-x64/bin/node \
        --dsh-root /home/winger/.local/dsh-0.1.2-rc.1 \
        --chrome /home/winger/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome

Environment fallbacks: NODE_BIN, DSH_ROOT, CHROME_BIN; otherwise use PATH for
Node/Chromium and the known local installation paths above. Each invocation
uses a private DSH_HOME, workdir, HOME and browser profile and two ephemeral
ports. This server is an isolated fixture, NOT a replacement for the live GUI.
No online config or installed DSH files are modified. Bootstrap credentials
stay in memory/private temporary logs and are never printed. The test only
observes the real sidebar button: it never clicks or invokes a directory
picker, and does not verify consent, mounting, FUSE, or gateway transport.
"""

import argparse
import contextlib
import json
import os
from pathlib import Path
import queue
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

import websocket

ROOT = Path(__file__).resolve().parents[1]
LOCAL_NODE = '/home/winger/.local/node-v22.23.1-linux-x64/bin/node'
LOCAL_DSH = '/home/winger/.local/dsh-0.1.2-rc.1'
LOCAL_CHROME = '/home/winger/.cache/ms-playwright/chromium-1234/chrome-linux64/chrome'
URL_RE = re.compile(r'https?://(?:127\.0\.0\.1|localhost|\[::1\]):\d+/[^\s\x1b<>\"\']*')
ANSI_RE = re.compile(r'\x1b\[[0-?]*[ -/]*[@-~]')
PLUGIN_RE = re.compile(r'browser[-_]workspace|dshgw-browser-workspace|挂载本地目录', re.I)
BUTTON = r'''(() => {
  const buttons = [...document.querySelectorAll('button')].filter(
    b => /^挂载本地目录(?:（读写）)?$/.test(b.textContent.trim()));
  const b = buttons.find(b => {
    const r = b.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && b.checkVisibility({checkOpacity:true, checkVisibilityCSS:true});
  });
  if (!b) return {found:false};
  // DSH's CSS-module sidebar footer, not an arbitrary matching page string.
  const footer = b.closest('[class*="_footerActions"]');
  const sidebar = footer && footer.closest('[class*="_root"]');
  return {found:!!sidebar, text:b.textContent.trim(), tag:b.tagName,
    sidebarFooter:!!footer, enabled:!b.disabled};
})()'''


def stop_group(proc):
    """Stop the entire fixture process group, including Chromium descendants."""
    if proc is None:
        return
    with contextlib.suppress(ProcessLookupError):
        os.killpg(proc.pid, signal.SIGTERM)
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        pass
    # The leader can exit before its children: always kill remaining group members.
    with contextlib.suppress(ProcessLookupError):
        os.killpg(proc.pid, signal.SIGKILL)
    proc.wait(timeout=5)


class CDP:
    def __init__(self, url):
        self.sock = websocket.create_connection(url, timeout=30, suppress_origin=True)
        self.serial = 0
        self.exceptions = []
        self.console_errors = []
        self.plugin_scripts = set()

    def call(self, method, params=None):
        self.serial += 1
        self.sock.send(json.dumps({'id': self.serial, 'method': method, 'params': params or {}}))
        deadline = time.monotonic() + 35
        while time.monotonic() < deadline:
            try:
                message = json.loads(self.sock.recv())
            except websocket.WebSocketTimeoutException:
                raise RuntimeError('CDP response timed out: ' + method) from None
            kind, data = message.get('method'), message.get('params', {})
            if kind == 'Runtime.exceptionThrown':
                self.exceptions.append(data.get('exceptionDetails', {}))
            elif kind == 'Runtime.consoleAPICalled' and data.get('type') in ('error', 'assert'):
                self.console_errors.append(data)
            elif kind == 'Log.entryAdded' and data.get('entry', {}).get('level') == 'error':
                self.console_errors.append(data['entry'])
            elif kind == 'Debugger.scriptParsed' and PLUGIN_RE.search(data.get('url', '')):
                self.plugin_scripts.add(data['url'])
            if message.get('id') == self.serial:
                if 'error' in message:
                    raise RuntimeError('CDP command failed: ' + method)
                return message.get('result', {})
        raise RuntimeError('CDP command timed out: ' + method)

    def button(self):
        result = self.call('Runtime.evaluate', {'expression': BUTTON, 'returnByValue': True})
        if 'exceptionDetails' in result:
            raise RuntimeError('sidebar DOM inspection raised a JavaScript exception')
        return result.get('result', {}).get('value', {})


def run(args):
    dsh = chrome = cdp = reader = None
    with tempfile.TemporaryDirectory(prefix='browser-workspace-ui-') as temporary:
        base = Path(temporary)
        home, dsh_home, workdir, profile = [base / name for name in ('home', 'dsh', 'workdir', 'chrome')]
        for path in (home, dsh_home, workdir, profile):
            path.mkdir(mode=0o700)
        patch = dsh_home / 'profiles/web/cordis.patch.yml'
        patch.parent.mkdir(parents=True)
        plugin = ROOT / 'cmd/dshgw/plugin/browser-workspace/index.js'
        if not plugin.is_file():
            raise RuntimeError('repository browser-workspace plugin is missing')
        # JSON is a YAML subset; no PyYAML dependency is required.
        patch.write_text(json.dumps([{'insert': [{'id': 'browser-workspace',
            'name': plugin.as_uri(), 'config': {}}]}]), encoding='utf-8')
        env = os.environ.copy()
        for name in tuple(env):
            if name.startswith('DSH_') or name in ('NODE_OPTIONS', 'NODE_PATH'):
                env.pop(name)
        env.update(DSH_HOME=str(dsh_home), HOME=str(home), NO_COLOR='1',
                   XDG_CONFIG_HOME=str(home / '.config'), XDG_CACHE_HOME=str(home / '.cache'),
                   XDG_DATA_HOME=str(home / '.local/share'))
        lines = queue.Queue()
        with (base / 'chrome.log').open('w') as chrome_log:
            try:
                dsh = subprocess.Popen([args.node, str(Path(args.dsh_root) / 'lib/bin.js'),
                    'web', '--port', '0', '--no-open'], cwd=workdir, env=env,
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                    text=True, start_new_session=True)

                def consume_stdout():
                    for line in dsh.stdout:
                        lines.put(line)

                reader = threading.Thread(target=consume_stdout, daemon=True)
                reader.start()
                deadline = time.monotonic() + args.timeout
                bootstrap = None
                while time.monotonic() < deadline:
                    if dsh.poll() is not None:
                        raise RuntimeError('isolated DSH exited before printing a bootstrap URL')
                    try:
                        line = ANSI_RE.sub('', lines.get(timeout=0.1))
                    except queue.Empty:
                        continue
                    for url in URL_RE.findall(line):
                        if re.search(r'[?&#]token=[^&#\s]+', url):
                            bootstrap = url
                            break
                    if bootstrap:
                        break
                if not bootstrap:
                    raise RuntimeError('timed out waiting for token-bearing DSH stdout bootstrap URL')
                chrome = subprocess.Popen([args.chrome, '--headless', '--no-sandbox',
                    '--disable-gpu', '--no-first-run', '--no-default-browser-check',
                    '--disable-background-networking', '--window-size=1440,1000',
                    '--remote-debugging-port=0', '--user-data-dir=' + str(profile), 'about:blank'],
                    cwd=workdir, env=env, stdout=chrome_log, stderr=chrome_log, start_new_session=True)
                portfile = profile / 'DevToolsActivePort'
                deadline = time.monotonic() + args.timeout
                while not portfile.exists():
                    if chrome.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError('Chromium failed to expose its debugging port')
                    time.sleep(0.05)
                port = int(portfile.read_text().splitlines()[0])
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with opener.open(f'http://127.0.0.1:{port}/json/list', timeout=10) as response:
                    target = next(t for t in json.load(response) if t['type'] == 'page')
                cdp = CDP(target['webSocketDebuggerUrl'])
                for domain in ('Page', 'Runtime', 'Log', 'Debugger'):
                    cdp.call(domain + '.enable')
                navigation = cdp.call('Page.navigate', {'url': bootstrap})
                if navigation.get('errorText'):
                    raise RuntimeError('Chromium failed to navigate to DSH bootstrap URL')
                deadline = time.monotonic() + args.timeout
                found_at = None
                button = {}
                while time.monotonic() < deadline:
                    if dsh.poll() is not None or chrome.poll() is not None:
                        raise RuntimeError('fixture exited while waiting for UI registration')
                    button = cdp.button()
                    if button.get('found'):
                        found_at = found_at or time.monotonic()
                        # Continue draining real JS events after initial React mount.
                        if time.monotonic() - found_at >= 2:
                            break
                    else:
                        found_at = None
                    time.sleep(0.1)
                # Do not output raw browser events: URLs/stacks can contain auth tokens.
                related = [event for event in cdp.exceptions + cdp.console_errors
                           if PLUGIN_RE.search(json.dumps(event, ensure_ascii=False))]
                result = {'button': button, 'pluginScriptURLsObserved': len(cdp.plugin_scripts),
                          'uncaughtJSExceptions': len(cdp.exceptions),
                          'consoleErrors': len(cdp.console_errors),
                          'pluginRelatedErrors': len(related), 'pickerInvoked': False}
                print(json.dumps(result, ensure_ascii=False, indent=2))
                if not button.get('found') or found_at is None or time.monotonic() - found_at < 2:
                    raise RuntimeError('real visible sidebar button 挂载本地目录 did not stabilize')
                if related or cdp.exceptions:
                    raise RuntimeError('browser reported plugin-related errors or uncaught JS exceptions')
                return result
            finally:
                if cdp:
                    with contextlib.suppress(Exception):
                        cdp.sock.close()
                try:
                    stop_group(chrome)
                finally:
                    stop_group(dsh)
                    if reader:
                        reader.join(timeout=5)
                    if dsh and dsh.stdout:
                        dsh.stdout.close()
    # TemporaryDirectory removes credentials, profiles and logs even on failure.


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--node', default=os.environ.get('NODE_BIN') or shutil.which('node') or LOCAL_NODE)
    parser.add_argument('--dsh-root', default=os.environ.get('DSH_ROOT') or LOCAL_DSH)
    parser.add_argument('--chrome', default=os.environ.get('CHROME_BIN') or shutil.which('chromium') or shutil.which('google-chrome') or LOCAL_CHROME)
    parser.add_argument('--timeout', type=float, default=45, help='deadline in seconds per startup/UI phase (default: 45)')
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error('--timeout must be positive')
    for value in (args.node, args.chrome):
        if not shutil.which(value):
            parser.error('Node/Chromium executable missing; use --node/--chrome or environment fallbacks')
    if not (Path(args.dsh_root) / 'lib/bin.js').is_file():
        parser.error('--dsh-root must contain the installed lib/bin.js')
    # SIGINT already raises KeyboardInterrupt; make SIGTERM run the same cleanup.
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    try:
        run(args)
    except KeyboardInterrupt:
        print('FAIL: interrupted; isolated fixture cleanup completed', file=sys.stderr)
        return 130
    except Exception as error:
        # Never print arbitrary exception strings (websocket/HTTP errors can embed URLs).
        safe = str(error) if type(error) is RuntimeError else type(error).__name__
        print('FAIL: ' + safe, file=sys.stderr)
        return 1
    print('PASS: real DSH browser-workspace sidebar UI; no picker/backend used; fixtures cleaned')
    return 0


if __name__ == '__main__':
    sys.exit(main())
