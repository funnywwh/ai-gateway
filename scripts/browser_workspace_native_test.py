#!/usr/bin/env python3
"""Real Chromium File System Access executor smoke test.

Uses a temporary browser profile and native origin-private filesystem handles.
It does NOT automate the OS directory consent dialog and does not establish
permission to any user's directory. No live gateway or GUI is modified.
"""
import argparse
import contextlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.request

import websocket

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = ROOT / 'cmd/dshgw/plugin/browser-workspace/client.js'
PAGE = b'''<!doctype html><meta charset="utf-8"><title>Browser workspace native FSA test</title>
<script>window.__ModuleLoader__={load({factory}){window.plugin=factory(()=>({}));}};</script>
<script src="/plugin.js"></script>'''
TEST = r'''(async()=>{
 const results=[];
 function check(ok,message){if(!ok)throw Error(message);results.push(message);}
 check(isSecureContext, 'secure loopback context');
 check(typeof showDirectoryPicker==='function', 'directory picker API present (not invoked)');
 const storageRoot=await navigator.storage.getDirectory();
 const name='browser-workspace-'+crypto.randomUUID();
 const root=await storageRoot.getDirectoryHandle(name,{create:true});
 const execute=plugin.createExecutor(root,{writable:true});
 try {
  await execute({op:'mkdir',path:'dir'});
  await execute({op:'create',path:'dir/data.bin',exclusive:true});
  const data=btoa(String.fromCharCode(0,255,128,10));
  const wrote=await execute({op:'write',path:'dir/data.bin',data,offset:0});
  check(wrote.bytes===4,'binary write commits 4 bytes');
  const read=await execute({op:'read',path:'dir/data.bin',offset:0,size:100});
  check(read.data===data && read.bytes===4,'native binary read roundtrip');
  await execute({op:'write',path:'dir/data.bin',data:btoa('Q'),offset:1});
  check((await execute({op:'read',path:'dir/data.bin',offset:1,size:1})).data===btoa('Q'),'offset write preserves bytes');
  await execute({op:'truncate',path:'dir/data.bin',size:2});
  check((await execute({op:'stat',path:'dir/data.bin'})).size===2,'native truncate');
  const listed=await execute({op:'list',path:'dir'});
  check(listed.entries.length===1 && listed.entries[0].size===2,'native directory metadata');
  let denied=false;try{await execute({op:'read',path:'../outside',size:1});}catch(e){denied=e.code==='EINVAL';}
  check(denied,'traversal fails before handle access');
  const ro=plugin.createExecutor(root);
  denied=false;try{await ro({op:'unlink',path:'dir/data.bin'});}catch(e){denied=e.code==='EROFS';}
  check(denied,'read-only executor rejects mutation');
  let renamed=false;
  try {await execute({op:'rename',path:'dir/data.bin',target:'dir/new.bin'});renamed=true;}
  catch(e){check(e.code==='ENOTSUP'||e.name==='NotSupportedError','unsupported native rename fails explicitly');}
  if(renamed)check((await execute({op:'stat',path:'dir/new.bin'})).size===2,'native move preserves content');
  await execute({op:'unlink',path:renamed?'dir/new.bin':'dir/data.bin'});
  await execute({op:'rmdir',path:'dir'});
  check((await execute({op:'list',path:''})).entries.length===0,'native removal leaves empty root');
  return {ok:true,results};
 } finally {await storageRoot.removeEntry(name,{recursive:true});}
})()'''

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/':
            body, kind = PAGE, 'text/html'
        elif self.path == '/plugin.js':
            body, kind = PLUGIN.read_bytes(), 'text/javascript'
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header('Content-Type', kind)
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_args):
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--chrome', default=os.environ.get('CHROME_BIN') or shutil.which('chromium') or shutil.which('google-chrome'))
    args = parser.parse_args()
    if not args.chrome:
        parser.error('set CHROME_BIN or --chrome to an installed Chromium binary')
    with tempfile.TemporaryDirectory(prefix='browser-workspace-chrome-') as temp:
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        serving = threading.Thread(target=server.serve_forever, daemon=True)
        serving.start()
        log = open(Path(temp) / 'chrome.log', 'w+')
        proc = subprocess.Popen([args.chrome, '--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check', '--remote-debugging-port=0', '--remote-allow-origins=*', '--user-data-dir='+temp, 'about:blank'], stdout=log, stderr=log)
        sock = None
        try:
            portfile = Path(temp) / 'DevToolsActivePort'
            deadline = time.monotonic()+20
            while not portfile.exists():
                if proc.poll() is not None or time.monotonic()>deadline:
                    log.seek(0)
                    raise RuntimeError('Chromium did not start: '+log.read()[-4000:])
                time.sleep(0.05)
            port = int(portfile.read_text().splitlines()[0])
            with urllib.request.urlopen(f'http://127.0.0.1:{port}/json/list', timeout=10) as resp:
                target = next(p for p in json.load(resp) if p['type']=='page')
            sock = websocket.create_connection(target['webSocketDebuggerUrl'], timeout=30)
            serial = 0
            def cdp(method, params):
                nonlocal serial
                serial += 1
                sock.send(json.dumps({'id':serial,'method':method,'params':params}))
                while True:
                    reply=json.loads(sock.recv())
                    if reply.get('id')==serial:
                        if 'error' in reply:
                            raise RuntimeError(reply['error'])
                        return reply.get('result',{})
            cdp('Page.navigate',{'url':f'http://127.0.0.1:{server.server_port}/'})
            deadline=time.monotonic()+15
            while True:
                value=cdp('Runtime.evaluate',{'expression':'typeof window.plugin === "object"','returnByValue':True})
                if value.get('result',{}).get('value'):
                    break
                if time.monotonic()>deadline:
                    raise RuntimeError('plugin failed to load')
                time.sleep(.05)
            reply=cdp('Runtime.evaluate',{'expression':TEST,'awaitPromise':True,'returnByValue':True})
            if 'exceptionDetails' in reply:
                raise RuntimeError(json.dumps(reply['exceptionDetails']))
            result=reply.get('result',{}).get('value')
            if not result or not result.get('ok'):
                raise RuntimeError(result)
            print(json.dumps(result,ensure_ascii=False,indent=2))
            print('PASS: native Chromium FSA executor; OS picker consent not tested')
        finally:
            if sock:
                sock.close()
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
            server.shutdown()
            server.server_close()
            serving.join(timeout=5)
            log.close()

if __name__ == '__main__':
    main()
