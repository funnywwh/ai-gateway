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
import base64
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
PLUGIN_RE = re.compile(r'browser[-_]workspace|dshgw-browser-workspace|浏览器工作区', re.I)
# The sidebar row is one compact button stacked above the ssh-workspace one: an icon span,
# the label 浏览器工作区 and an optional state note. It is matched on the label, not on
# exact text; the row's data-dshgw-state phase attribute is what a real browser run can
# assert about the status without parsing the note.
BUTTON = r'''(() => {
  const buttons = [...document.querySelectorAll('button')].filter(
    b => b.textContent.includes('浏览器工作区'));
  const b = buttons.find(b => {
    const r = b.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && b.checkVisibility({checkOpacity:true, checkVisibilityCSS:true});
  });
  if (!b) return {found:false};
  // DSH's CSS-module sidebar footer, not an arbitrary matching page string.
  const footer = b.closest('[class*="_footerActions"]');
  const sidebar = footer && footer.closest('[class*="_root"]');
  const box = b.getBoundingClientRect();
  const dialog = document.querySelector('[data-dshgw-dialog="browser-workspace"]');
  return {found:!!sidebar, text:b.textContent.trim(), tag:b.tagName,
    sidebarFooter:!!footer, enabled:!b.disabled,
    state:b.dataset.dshgwState || null,
    dialogOpen:!!dialog,
    dialogText:dialog ? String(dialog.textContent || '').slice(0, 200) : null,
    // Where the stacking rule has to land: the shell's foot, seen from the row.
    footerDirection:footer ? getComputedStyle(footer).flexDirection : null,
    parentChain:(() => { const chain = []; let node = b;
      while (node && chain.length < 4) { chain.push(node.tagName + '.' + String(node.className || '').slice(0, 40)); node = node.parentElement; }
      return chain; })(),
    x: Math.round(box.left + box.width / 2), y: Math.round(box.top + box.height / 2)};
})()'''

# The OS dialog cannot be driven from here, so the picker is replaced by a spy that records
# the call — and whether the click's transient user activation was still active at that
# moment, which is exactly what showDirectoryPicker() needs — and then rejects with
# AbortError, like a person cancelling the dialog. One click must reach it: a consent step
# or a blocking dialog in front of the picker would spend the activation first.
PICKER_SPY = r'''(() => {
  window.__bwPicker = { calls: 0, active: null, mode: null, clicks: 0, target: null };
  document.addEventListener('click', (event) => {
    window.__bwPicker.clicks += 1;
    window.__bwPicker.target = event.target.tagName + ':' + String(event.target.textContent || '').slice(0, 24);
  }, true);
  window.showDirectoryPicker = (options) => {
    window.__bwPicker.calls += 1;
    window.__bwPicker.active = navigator.userActivation ? navigator.userActivation.isActive : null;
    window.__bwPicker.mode = options && options.mode;
    return Promise.reject(Object.assign(new Error('smoke: dialog cancelled'), { name: 'AbortError' }));
  };
  return true;
})()'''
PICKER_STATE = 'window.__bwPicker'

# DSH's own first-run notice ("Internal Testing Notice") is modal: its mask covers the whole
# page, sidebar foot included, until the person clicks Continue. A fresh fixture profile has
# it, so the smoke test dismisses it the same way a person does and only then clicks the row.
DIALOG_BUTTON = r'''(() => {
  const mask = document.querySelector('[class*="_mask_"]');
  const dialog = mask && mask.parentElement;
  if (!dialog) return {found: false};
  const buttons = [...dialog.querySelectorAll('button')].filter(b => {
    const r = b.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  });
  const button = buttons.find(b => /(later|continue|ok|got it|知道了|继续|关闭|稍后)/i.test(b.textContent.trim()))
    || (buttons.length === 1 ? buttons[0] : null);
  if (!button) return {found: false, buttons: buttons.map(b => String(b.textContent || '').slice(0, 24))};
  const box = button.getBoundingClientRect();
  return {found: true, label: button.textContent.trim(), x: Math.round(box.left + box.width / 2), y: Math.round(box.top + box.height / 2)};
})()'''

# Where to click: the row itself must be the hit target of the point, otherwise a modal mask
# or another overlay would swallow the click (a human could not click it either).
HIT_POINT = r'''(() => {
  const button = [...document.querySelectorAll('button')].find(b => b.textContent.includes('浏览器工作区'));
  if (!button) return {found: false, blocker: 'no row'};
  const box = button.getBoundingClientRect();
  for (let fx = 0.5; fx >= 0.1; fx -= 0.2) {
    for (let fy = 0.5; fy >= 0.1; fy -= 0.2) {
      const x = box.left + box.width * fx, y = box.top + box.height * fy;
      const hit = document.elementFromPoint(x, y);
      if (hit === button || button.contains(hit)) return {found: true, x: Math.round(x), y: Math.round(y)};
    }
  }
  const blocker = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2);
  const mask = document.querySelector('[class*="_mask_"]');
  const dialog = mask && mask.parentElement;
  return {found: false, blocker: blocker ? blocker.tagName + '.' + String(blocker.className || '') : null,
    dialogText: dialog ? String(dialog.textContent || '').slice(0, 200) : null,
    dialogButtons: dialog ? [...dialog.querySelectorAll('button')].map(b => String(b.textContent || '').slice(0, 24)) : null};
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

    def dialog_button(self):
        """The visible dismissal button of DSH's own modal notice, if one is open."""
        result = self.call('Runtime.evaluate', {'expression': DIALOG_BUTTON, 'returnByValue': True})
        if 'exceptionDetails' in result:
            raise RuntimeError('dialog inspection raised a JavaScript exception')
        return result.get('result', {}).get('value', {})

    def point(self):
        """A point inside the row that the row itself receives, or the blocker at its centre."""
        result = self.call('Runtime.evaluate', {'expression': HIT_POINT, 'returnByValue': True})
        if 'exceptionDetails' in result:
            raise RuntimeError('sidebar hit test raised a JavaScript exception')
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
                if not button.get('found') or found_at is None or time.monotonic() - found_at < 2:
                    raise RuntimeError('real visible sidebar button 浏览器工作区 did not stabilize')
                if related or cdp.exceptions:
                    raise RuntimeError('browser reported plugin-related errors or uncaught JS exceptions')
                # A fresh profile may open a modal whose mask covers the whole page (including
                # this row), so dismiss it before clicking and then verify from the DOM that the
                # row itself — not some overlay — is the hit target of the point we click.
                # A fresh profile opens DSH's own modals (testing notice, then "add an API
                # key"); each one's mask covers the sidebar until a person dismisses it.
                dismissed = []
                hit = cdp.point()
                for _ in range(4):
                    if hit.get('found'):
                        break
                    dialog = cdp.dialog_button()
                    if not dialog.get('found'):
                        break
                    for kind in ('mousePressed', 'mouseReleased'):
                        cdp.call('Input.dispatchMouseEvent', {'type': kind, 'x': dialog['x'],
                                                              'y': dialog['y'], 'button': 'left', 'clickCount': 1})
                    dismissed.append(dialog.get('label'))
                    time.sleep(0.5)
                    hit = cdp.point()
                if not hit.get('found'):
                    raise RuntimeError('the sidebar row is covered and cannot be clicked: %r' % (hit,))
                # One trusted click must reach the picker, with the click's user activation
                # still active and without any consent step in front of it.
                cdp.call('Runtime.evaluate', {'expression': PICKER_SPY, 'returnByValue': True})
                for kind in ('mousePressed', 'mouseReleased'):
                    cdp.call('Input.dispatchMouseEvent', {'type': kind, 'x': hit['x'],
                                                          'y': hit['y'], 'button': 'left', 'clickCount': 1})
                time.sleep(0.5)
                picker = cdp.call('Runtime.evaluate', {'expression': PICKER_STATE, 'returnByValue': True})
                picker = picker.get('result', {}).get('value', {})
                settled = cdp.button()
                result = {'button': button, 'pluginScriptURLsObserved': len(cdp.plugin_scripts),
                          'uncaughtJSExceptions': len(cdp.exceptions),
                          'consoleErrors': len(cdp.console_errors),
                          'pluginRelatedErrors': len(related), 'picker': picker,
                          'clickedPoint': hit, 'dismissedDialogs': dismissed,
                          'buttonAfterCancel': settled.get('text'),
                          'stateAfterCancel': settled.get('state'),
                          'dialogAfterCancel': settled.get('dialogOpen')}
                print(json.dumps(result, ensure_ascii=False, indent=2))
                if picker.get('calls') != 1 or picker.get('active') is not True or picker.get('mode') != 'readwrite':
                    raise RuntimeError('one click did not reach the picker with user activation: %r' % (picker,))
                # The row carries its own status as a phase attribute; before the click
                # nothing is mounted, so the phase must be idle.
                if button.get('state') != 'idle':
                    raise RuntimeError('the sidebar row does not report an idle phase: %r' % (button,))
                # The shell's foot is a flex row by default, which would put this row and the
                # ssh one side by side in half the width each. The stacking rule must have
                # landed on the shell's own container, not on the wrapper around the row.
                if button.get('footerDirection') != 'column':
                    raise RuntimeError('the sidebar foot was not stacked into rows: %r' % (button,))
                if settled.get('text') != button.get('text') or settled.get('enabled') is not True:
                    raise RuntimeError('cancelling the picker changed the sidebar row: %r' % (settled,))
                # A cancelled picker mounted nothing and failed at nothing: the mount dialog
                # it opened must be gone again, and the phase must not have moved.
                if settled.get('dialogOpen') or settled.get('state') != button.get('state'):
                    raise RuntimeError('cancelling the picker left the mount dialog up: %r' % (settled,))
                if cdp.exceptions or cdp.console_errors:
                    raise RuntimeError('the single-click picker raised browser errors')
                if args.screenshot:
                    # Optional human-review artifact: the fixture page only, never a real session.
                    shot = cdp.call('Page.captureScreenshot', {'format': 'png'})
                    Path(args.screenshot).write_bytes(base64.b64decode(shot['data']))
                    print('screenshot:', args.screenshot)
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
    parser.add_argument('--screenshot', default='', help='write a PNG of the verified sidebar row here (optional)')
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
    print('PASS: real DSH browser-workspace sidebar UI; the row reports its phase; one click reached the picker with user activation; cancelling it left the row and closed the mount dialog; no backend used; fixtures cleaned')
    return 0


if __name__ == '__main__':
    sys.exit(main())
