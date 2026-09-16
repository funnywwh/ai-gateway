#!/usr/bin/env bash
set -euo pipefail

NODE=${DSHGW_NODE:-/opt/dsh/node/bin/node}
if [[ -n ${DSHGW_BIN_JS:-} ]]; then
  DSH_BIN=$DSHGW_BIN_JS
elif [[ -n ${DSHGW_DSH_ROOT:-} ]]; then
  DSH_BIN="$DSHGW_DSH_ROOT/lib/bin.js"
else
  DSH_BIN=/opt/dsh/current/lib/bin.js
fi
NODE_DIR=$(dirname "$NODE")
COREPACK=${DSHGW_COREPACK:-$NODE_DIR/corepack}
DEST=${DSHGW_TEMPLATE_HOME:-/opt/dshgw/share/template-home}
TEST_MODE=${DSHGW_TEMPLATE_TEST_MODE:-0}

if [[ ${EUID} -ne 0 ]]; then
  [[ "$TEST_MODE" == 1 && "$DEST" == /tmp/* ]] || { echo "prepare-template: run as root (or set DSHGW_TEMPLATE_TEST_MODE=1 for a /tmp-only verification)" >&2; exit 1; }
fi
[[ "$DEST" == /* ]] || { echo "prepare-template: template destination must be absolute" >&2; exit 1; }
[[ -x "$NODE" ]] || { echo "prepare-template: missing executable Node: $NODE" >&2; exit 1; }
[[ -r "$DSH_BIN" ]] || { echo "prepare-template: missing dsh launcher: $DSH_BIN" >&2; exit 1; }
[[ -x "$COREPACK" ]] || { echo "prepare-template: missing Corepack next to Node ($COREPACK); set DSHGW_COREPACK" >&2; exit 1; }
mkdir -p "$(dirname "$DEST")"
stage=$(mktemp -d "${DEST}.stage.XXXXXX")
wrapper=$(mktemp -d)
cleanup(){ rm -rf "$stage" "$wrapper"; }
trap cleanup EXIT
cat >"$wrapper/pnpm" <<EOF
#!/bin/sh
exec "$COREPACK" pnpm "\$@"
EOF
chmod 0755 "$wrapper/pnpm"
mkdir -p "$stage"
HOME="$stage" DSH_HOME="$stage" "$NODE" "$DSH_BIN" --profile web --dump-config >/dev/null
PATH="$wrapper:$NODE_DIR:/usr/bin:/bin" COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
  HOME="$stage" DSH_HOME="$stage" "$NODE" "$DSH_BIN" plugin --profile web add --save-exact dsh-browser-fs@0.2.0
BROWSER_FS_INTEGRITY='sha512-K6h9UjBtV3AxEHzUdPD4XitCigRDXRo7lhC5JfuUZakSU73JQl06N+/DO652iq76pmTVdjIRZ1j/SVqQ4H3r+g=='
grep -F -- "$BROWSER_FS_INTEGRITY" "$stage/profiles/web/pnpm-lock.yaml" >/dev/null || {
  echo "prepare-template: dsh-browser-fs lockfile integrity mismatch" >&2
  exit 1
}
"$NODE" - "$stage/profiles/web/package.json" <<'NODE'
const fs=require('node:fs');const p=process.argv[2];const x=JSON.parse(fs.readFileSync(p));
if(x.dependencies?.['dsh-browser-fs']!=='0.2.0') throw new Error('browser-fs is not exactly pinned to 0.2.0');
if(!x.dsh?.profile?.bundles?.includes('dsh-browser-fs')) throw new Error('browser-fs bundle was not activated');
NODE
rm -rf "$stage/sessions" "$stage/storages" "$stage/.credentials.yaml"
if [[ ${EUID} -eq 0 ]]; then
  chown -R root:root "$stage"
fi
find "$stage" -type d -exec chmod go-w {} +
old="${DEST}.old"
rm -rf "$old"
had_old=0
if [[ -e "$DEST" || -L "$DEST" ]]; then
  mv "$DEST" "$old"
  had_old=1
fi
if ! mv "$stage" "$DEST"; then
  if [[ $had_old == 1 ]]; then mv "$old" "$DEST"; fi
  exit 1
fi
trap - EXIT
rm -rf "$wrapper" "$old"
echo "prepared $DEST (dsh-browser-fs 0.2.0 pinned; tenant first boot needs no network)"
