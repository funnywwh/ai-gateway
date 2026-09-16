#!/usr/bin/env bash
# M51 operator handoff for this repository's local :8088 deployment.
# Does not edit aigw configuration, update aigw binaries, create tenants,
# enable boot-time services, reload nginx, or expose a new public listener.
set -euo pipefail

usage(){
  echo 'usage: bash scripts/dshgw_stage2.sh --confirm-explicit-grants --confirm-restart-aigw'
  echo '   or: bash scripts/dshgw_stage2.sh --verify-and-start  # after a confirmed successful aigw restart'
  echo 'Run in the host root terminal after backing up/changing auth.default_grant to none.'
  echo 'Restarts the existing local aigw as winger, updates dshgw files, then starts only its loopback service.'
}
if [[ ${1:-} == --help ]]; then usage; exit 0; fi
restart_aigw=1
if [[ $# == 1 && $1 == --verify-and-start ]]; then
  restart_aigw=0
elif [[ $# != 2 || $1 != --confirm-explicit-grants || $2 != --confirm-restart-aigw ]]; then
  usage >&2; exit 2
fi
[[ $EUID == 0 ]] || { echo 'stage 2: run in the operator host root terminal; no automatic sudo' >&2; exit 1; }
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BIN=/opt/dshgw/bin/dshgw
CFG=/etc/dshgw/config.yaml
if systemctl is-active --quiet dshgw.service; then
  echo 'stage 2: dshgw is already running; stop and inspect instead of overwriting a live binary' >&2
  exit 1
fi
workers=$(systemctl list-units --state=active --no-legend 'dsh-worker@*.service')
[[ -z "$workers" ]] || { echo 'stage 2: existing active workers detected; use a planned maintenance procedure' >&2; exit 1; }
listeners=$(ss -H -ltn 'sport = :3099')
[[ -z "$listeners" ]] || { echo 'stage 2: port 3099 is occupied; do not kill it blindly' >&2; exit 1; }

# Pin the restart to the known local launcher and real process identity. Never
# display raw command lines, environment contents, or the configuration body.
python3 - "$ROOT" <<'PY'
import os, pwd, re, stat, sys
from pathlib import Path
root=Path(sys.argv[1])
config=root/'config.yaml'
if not stat.S_ISREG(config.lstat().st_mode): raise SystemExit('stage 2: config must be a regular non-symlink file')
lines=config.read_text().splitlines()
starts=[i for i,line in enumerate(lines) if re.fullmatch(r'auth:\s*(?:#.*)?',line)]
if len(starts)!=1: raise SystemExit('stage 2: expected one root auth block; inspect config locally')
body=[]
for line in lines[starts[0]+1:]:
    if line and not line[0].isspace() and not line.startswith('#'): break
    body.append(line)
grants=[re.fullmatch(r'  default_grant:\s*(\w+)\s*(?:#.*)?',line) for line in body]
grants=[match.group(1) for match in grants if match]
if grants!=['none']: raise SystemExit('stage 2: auth.default_grant must already be none; no automatic policy change')
pid=int((root/'data/aigw-local.pid').read_text().strip())
process=Path('/proc')/str(pid)
if process.stat().st_uid!=pwd.getpwnam('winger').pw_uid: raise SystemExit('stage 2: aigw process is not owned by winger')
if os.readlink(process/'exe')!=str(root/'bin/aigw'): raise SystemExit('stage 2: running aigw executable differs from local launcher')
argv=[os.fsdecode(value) for value in (process/'cmdline').read_bytes().split(b'\0') if value]
configs=[]
for i,field in enumerate(argv):
    if field in ('--config','-config') and i+1<len(argv): configs.append(argv[i+1])
    elif field.startswith('--config='): configs.append(field.split('=',1)[1])
if configs!=[str(config)]: raise SystemExit('stage 2: running aigw uses a different config; do not restart it with guessed settings')
env={part.split(b'=',1)[0]:part.split(b'=',1)[1] for part in (process/'environ').read_bytes().split(b'\0') if b'=' in part}
if any(name.startswith(b'GW_') for name in env): raise SystemExit('stage 2: running aigw has GW_ overrides; preserve/review them manually')
print('PASS local aigw identity/config preflight; configuration on disk is none')
PY

if [[ $restart_aigw == 1 ]]; then
# Installer is idempotent for valid selected Node/DSH links and does not restart
# services; the already-prepared browser-fs template is retained.
bash "$ROOT/deploy/dshgw/install.sh"
log_offset=$(stat -c %s "$ROOT/data/aigw-local.log")
echo 'Restarting existing local aigw (brief model-request interruption) as winger...'
restart_log=$(mktemp /root/dshgw-e2e/stage2-restart.XXXXXX)
if ! runuser -u winger -- env -u GW_AUTH_DEFAULT_GRANT CONFIG="$ROOT/config.yaml" BIN="$ROOT/bin/aigw" LISTEN=:8088 \
  bash "$ROOT/scripts/local-run.sh" restart >"$restart_log" 2>&1; then
  echo "stage 2: aigw restart failed; inspect PRIVATE diagnostics locally: $restart_log" >&2
  echo 'No dshgw listener was started; do not share the raw restart log or config.' >&2
  exit 1
fi
echo 'PASS local aigw restarted; raw diagnostics kept private'
python3 - "$ROOT/data/aigw-local.log" "$log_offset" <<'PY'
import re, sys
with open(sys.argv[1],'rb') as file:
    file.seek(int(sys.argv[2]))
    text=file.read(2<<20).decode('utf-8',errors='replace')
if not re.search(r'\bdefault_grant=none\b|"default_grant"\s*:\s*"none"',text):
    raise SystemExit('stage 2: new aigw startup did not confirm default_grant=none; inspect private log locally')
print('PASS newly started aigw reports auth.default_grant=none')
PY
else
  echo 'Resuming after operator-confirmed aigw restart; skipping installation and restart.'
fi

for key in /root/dshgw-e2e/a.key /root/dshgw-e2e/b.key; do
  "$BIN" --config "$CFG" contract --key-file "$key" aigw
done
PYTHONDONTWRITEBYTECODE=1 python3 - "$ROOT/scripts" <<'PY'
import json, sys
sys.path.insert(0,sys.argv[1])
from dshgw_host_acceptance import Failure, Transport, check, read_key
try:
    transport=Transport()
    for name in ('a','b'):
        key=read_key('/root/dshgw-e2e/'+name+'.key')
        status,_,body=transport.request('http://192.168.190.86:8088/v1/models',headers={'Authorization':'Bearer '+key},edge=False)
        check(status==200,'test Key '+name+' no longer authenticates after policy change')
        ids={item['id'] for item in json.loads(body)['data']}
        check('deepseek-flash' in ids,'test Key '+name+' lacks explicit deepseek-flash grant/availability; gateway stays stopped')
        print('PASS test Key '+name+': deepseek-flash available under default_grant=none')
except (Failure,OSError,KeyError,ValueError) as error:
    raise SystemExit(str(error) if isinstance(error,Failure) else 'stage 2: model-list verification failed; inspect locally') from None
PY

systemctl start dshgw.service
systemctl is-active dshgw.service
portal=$("$BIN" --config "$CFG" login-url)
authority=${portal#https://}; authority=${authority%/}
port=${authority##*:}
code=$(curl --noproxy '*' --silent --show-error --fail --retry 10 --retry-connrefused --retry-delay 1 \
  --max-time 5 --output /dev/null --write-out '%{http_code}' \
  --header "Host: $authority" --header "X-DSHGW-Port: $port" http://127.0.0.1:3099/)
[[ $code == 200 ]] || { echo "stage 2: loopback portal returned $code, expected 200" >&2; exit 1; }
echo 'PASS loopback gateway portal HTTP 200'
echo 'Stage 2 complete: no public nginx configuration generated/reloaded and no tenants created.'
