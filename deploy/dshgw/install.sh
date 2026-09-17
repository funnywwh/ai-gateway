#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NODE_SOURCE=${NODE_SOURCE:-/home/winger/.local/node-v22.23.1-linux-x64}
DSH_SOURCE=${DSH_SOURCE:-/home/winger/.local/dsh-0.1.2-rc.1}
DSH_VERSION=${DSH_VERSION:-0.1.2-rc.1}
if [[ ${EUID} -ne 0 ]]; then echo "install: run as root" >&2; exit 1; fi
[[ -x "$ROOT/bin/dshgw" ]] || { echo "install: build bin/dshgw first (make dshgw-build)" >&2; exit 1; }
[[ -d "$NODE_SOURCE" && -x "$NODE_SOURCE/bin/node" && -d "$DSH_SOURCE" && -r "$DSH_SOURCE/lib/bin.js" ]] || {
  echo "install: NODE_SOURCE or DSH_SOURCE invalid" >&2
  exit 1
}
[[ "$DSH_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]] || {
  echo "install: DSH_VERSION must be a single safe release name" >&2
  exit 1
}
getent group dshgw >/dev/null || groupadd --system dshgw
id -u dshgw >/dev/null 2>&1 || useradd --system --gid dshgw --home-dir /var/lib/dshgw --shell /usr/sbin/nologin dshgw
install -d -m 0755 /opt/dsh /opt/dsh/releases /opt/dshgw/bin /opt/dshgw/share/dsh-plugin
# Search-only access through shared parents is required by independent tenant
# UIDs. Never add a worker to the gateway group or broaden private leaf modes.
install -d -o root -g dshgw -m 0751 /var/lib/dshgw
install -d -o root -g root -m 0711 /var/lib/dshgw/tenants
install -d -o dshgw -g dshgw -m 0700 /var/lib/dshgw/gateway
install -d -o root -g dshgw -m 0750 /var/lib/dshgw/handshake /etc/dshgw /etc/dshgw/tenants
install -d -m 0755 /srv/dsh /etc/nginx/conf.d/dshgw
node_dest="/opt/dsh/$(basename "$NODE_SOURCE")"
dsh_dest="/opt/dsh/releases/$DSH_VERSION"
if [[ -e "$node_dest" || -L "$node_dest" ]]; then
  [[ -d "$node_dest" && ! -L "$node_dest" ]] || { echo "install: existing Node destination is not a real directory: $node_dest" >&2; exit 1; }
else
  cp -a --reflink=auto "$NODE_SOURCE" "$node_dest"
fi
if [[ -e "$dsh_dest" || -L "$dsh_dest" ]]; then
  [[ -d "$dsh_dest" && ! -L "$dsh_dest" ]] || { echo "install: existing DSH destination is not a real directory: $dsh_dest" >&2; exit 1; }
else
  cp -a --reflink=auto "$DSH_SOURCE" "$dsh_dest"
fi
# Existing runtime selections may have been changed deliberately by the DSH
# upgrade flow. Preserve valid selections instead of silently downgrading them
# to this install's defaults; reject broken or out-of-tree links.
ensure_runtime_link(){
  local link=$1 desired=$2 required=$3 resolved
  if [[ -L "$link" ]]; then
    [[ -d "$link" ]] || { echo "install: runtime link is broken: $link (restore a directory under /opt/dsh before retrying)" >&2; exit 1; }
    resolved=$(readlink -f -- "$link") || { echo "install: cannot resolve runtime link: $link" >&2; exit 1; }
    case "$resolved" in
      /opt/dsh/*) ;;
      *) echo "install: runtime link escapes /opt/dsh: $link -> $resolved" >&2; exit 1 ;;
    esac
    [[ -e "$resolved/$required" ]] || { echo "install: runtime link has no $required: $link -> $resolved" >&2; exit 1; }
    echo "install: preserving existing runtime link $link -> $resolved"
    return 0
  fi
  [[ ! -e "$link" ]] || { echo "install: refusing to replace non-symlink $link" >&2; exit 1; }
  ln -s -- "$desired" "$link"
}
chown -R root:root "$node_dest" "$dsh_dest"
find "$node_dest" "$dsh_dest" -type d -exec chmod go-w {} +
find "$node_dest" "$dsh_dest" -type f -exec chmod go-w {} +
ensure_runtime_link /opt/dsh/node "$node_dest" bin/node
ensure_runtime_link /opt/dsh/current "$dsh_dest" lib/bin.js
install -m 0755 "$ROOT/bin/dshgw" /opt/dshgw/bin/dshgw
install -m 0644 "$ROOT/cmd/dshgw/plugin/picker-clamp.js" /opt/dshgw/share/dsh-plugin/picker-clamp.js
install -m 0644 "$ROOT/cmd/dshgw/plugin/picker-clamp.test.mjs" /opt/dshgw/share/dsh-plugin/picker-clamp.test.mjs
install -m 0644 "$ROOT/deploy/dshgw/dsh-worker@.service" /etc/systemd/system/dsh-worker@.service
install -m 0644 "$ROOT/deploy/dshgw/dsh-worker-bwrap@.service" /etc/systemd/system/dsh-worker-bwrap@.service
install -m 0644 "$ROOT/deploy/dshgw/dsh-workers.slice" /etc/systemd/system/dsh-workers.slice
install -m 0644 "$ROOT/deploy/dshgw/dshgw.service" /etc/systemd/system/dshgw.service
install -m 0644 "$ROOT/deploy/dshgw/dshgw-admin.service" /etc/systemd/system/dshgw-admin.service
install -m 0644 "$ROOT/deploy/dshgw/dshgw.logrotate" /etc/logrotate.d/dshgw
if [[ ! -e /etc/dshgw/config.yaml ]]; then install -m 0640 -o root -g dshgw "$ROOT/deploy/dshgw/config.example.yaml" /etc/dshgw/config.yaml; fi
if [[ ! -e /var/lib/dshgw/registry.json ]]; then printf '{"version":1,"tenants":[]}\n' >/var/lib/dshgw/registry.json; chown dshgw:dshgw /var/lib/dshgw/registry.json; chmod 0600 /var/lib/dshgw/registry.json; fi
if [[ ! -e /var/lib/dshgw/keys.map ]]; then : >/var/lib/dshgw/keys.map; chown root:dshgw /var/lib/dshgw/keys.map; chmod 0640 /var/lib/dshgw/keys.map; fi
systemctl daemon-reload
echo "installed dshgw runtime; review /etc/dshgw/config.yaml, then run deploy/dshgw/prepare-template.sh"
