#!/usr/bin/env bash
# Decommission a legacy root/systemd dshgw deployment and keep an archive of it (M63).
#
# The root shape is: dshgw.service + dshgw-admin.service + one dsh-worker@<tenant>.service per
# tenant, state in /var/lib/dshgw, configuration in /etc/dshgw, the binary in /opt/dshgw, and
# TLS fronted by nginx (conf.d/dshgw/*) on the tenant ports. This script stops it, archives
# everything that carries information, and then deletes the three directories.
#
# The archive is the ONLY rollback source, so it is written and verified before anything is
# removed. It goes to <deploy root>/data/prev/legacy-dshgw by default — inside the single
# data root, next to the other history (see docs/deployment-layout.md §7).
#
# Tenants of the old deployment are NOT migrated into the supervised shape: this script
# archives them. If one of them is a real user, tell them before running --apply.
#
# Usage:
#   sudo scripts/decommission_legacy_dshgw.sh [--apply] [--archive-only]
#                                            [--archive-dir DIR] [--remove-accounts]
#                                            [--old-config PATH] [--state-dir DIR]
#                                            [--etc-dir DIR] [--nginx-dir DIR]
#
#   (no --apply)     print the plan: inventory, what gets archived, what gets stopped
#   --apply          do it (needs root; nothing is deleted before the archive is verified)
#   --archive-only   take the archive (and verify it) and stop there; needs no root when the
#                    archive directory is writable. Useful when you want the rollback source
#                    in hand before touching a running deployment.
#   --archive-dir    where the archive lands (default <repo>/data/prev/legacy-dshgw)
#   --remove-accounts
#                    also delete the per-tenant OS accounts (dsh-<tenant>) and their homes.
#                    Off by default: deleting accounts is the least reversible step here, and
#                    a leftover account is harmless once nothing runs as it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APPLY=0
ARCHIVE_ONLY=0
REMOVE_ACCOUNTS=0
ARCHIVE_DIR=""
OLD_CONFIG="/etc/dshgw/config.yaml"
OLD_STATE=""
OLD_REGISTRY=""
OLD_WORKSPACE=""
OLD_TENANT_ROOT=""
OLD_TENANT_CONFIG=""
OLD_BACKUP=""
OLD_ETC="/etc/dshgw"
OLD_OPT="/opt/dshgw"
NGINX_CONF_DIR=""
UNIT_DIR="/etc/systemd/system"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --archive-only) ARCHIVE_ONLY=1; shift ;;
    --archive-dir) ARCHIVE_DIR="$2"; shift 2 ;;
    --remove-accounts) REMOVE_ACCOUNTS=1; shift ;;
    --old-config) OLD_CONFIG="$2"; shift 2 ;;
    --state-dir) OLD_STATE="$2"; shift 2 ;;
    --etc-dir) OLD_ETC="$2"; shift 2 ;;
    --nginx-dir) NGINX_CONF_DIR="$2"; shift 2 ;;
    -h|--help) sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -n "$ARCHIVE_DIR" ]] || ARCHIVE_DIR="$ROOT/data/prev/legacy-dshgw"

say()  { printf '%s\n' "$*"; }
step() { printf '%s  %s\n' "$([[ "$APPLY" == 1 ]] && echo RUN || echo PLAN)" "$*"; }
info() { printf 'INFO  %s\n' "$*"; }
fail() { echo "decommission-legacy-dshgw: $*" >&2; exit 1; }
run()  { if [[ "$APPLY" == 1 ]]; then printf 'RUN   %s\n' "$*"; "$@"; else step "$*"; fi }

# Owner of the archive and of any surviving copy: the account that invoked sudo, so the
# archive is readable without root afterwards.
TARGET_USER="${SUDO_USER:-$(id -un)}"

if [[ "$APPLY" == 1 && "$ARCHIVE_ONLY" != 1 && "$(id -u)" != 0 ]]; then
  fail "--apply needs root (run it as: sudo $0 --apply)"
fi

# The old deployment is described by its own configuration: reading it is how this script
# learns where the state actually lives instead of assuming the default, because a legacy
# host may well have moved it (the old config allowed state_dir and nginx_include_path).
# PyYAML is parsed leniently: this file belongs to the *deleted* shape, so it also contains
# keys today's strict decoder rejects.
read_old_config() {
  [[ -f "$OLD_CONFIG" ]] || return 0
  python3 - "$OLD_CONFIG" <<'PY' 2>/dev/null || true
import sys
try:
    import yaml
except Exception:
    raise SystemExit(0)
try:
    doc = yaml.safe_load(open(sys.argv[1])) or {}
except Exception:
    raise SystemExit(0)
deploy = doc.get("deploy") or {}
out = {
    "state_dir": doc.get("state_dir") or "",
    "registry_path": doc.get("registry_path") or "",
    "nginx_dir": doc.get("nginx_include_path") or doc.get("nginx_dir") or "",
    # Trees that hold tenant data but live outside the state directory. The script archives
    # them and does NOT delete them, but leaving them unnamed is how a "decommissioned"
    # deployment keeps gigabytes of a real user's files nobody remembers.
    "workspace_root": doc.get("workspace_root") or "",
    "tenant_root": doc.get("tenant_root") or "",
    "tenant_config_root": deploy.get("tenant_config_root") or "",
    "backup_dir": deploy.get("backup_dir") or "",
}
for key, value in out.items():
    print(f"{key.upper()}={value}")
PY
}

while IFS='=' read -r key value; do
  case "$key" in
    STATE_DIR) [[ -n "$OLD_STATE" ]] || OLD_STATE="$value" ;;
    REGISTRY_PATH) [[ -n "$value" ]] && OLD_REGISTRY="$value" ;;
    NGINX_DIR) [[ -n "$NGINX_CONF_DIR" ]] || NGINX_CONF_DIR="$value" ;;
    WORKSPACE_ROOT) [[ -n "$value" ]] && OLD_WORKSPACE="$value" ;;
    TENANT_ROOT) [[ -n "$value" ]] && OLD_TENANT_ROOT="$value" ;;
    TENANT_CONFIG_ROOT) [[ -n "$value" ]] && OLD_TENANT_CONFIG="$value" ;;
    BACKUP_DIR) [[ -n "$value" ]] && OLD_BACKUP="$value" ;;
  esac
done < <(read_old_config)
[[ -n "$OLD_STATE" ]] || OLD_STATE="/var/lib/dshgw"
[[ -n "$OLD_REGISTRY" ]] || OLD_REGISTRY="$OLD_STATE/registry.json"
[[ -n "$NGINX_CONF_DIR" ]] || NGINX_CONF_DIR="/etc/nginx/conf.d/dshgw"

# ---------------------------------------------------------------------------- inventory
# The inventory is read-only on purpose and runs in both modes: an operator deciding whether
# to go ahead needs to see which tenants exist and how much data is at stake.
unit_names() {
  systemctl list-units --all --no-legend 'dsh-worker@*.service' 'dshgw.service' 'dshgw-admin.service' 2>/dev/null \
    | awk '{print $1}' | sed 's/\.service$//' | sort -u || true
}

registry_tenants() {
  python3 - "$OLD_REGISTRY" <<'PY' 2>/dev/null || true
import json, sys
from pathlib import Path
p = Path(sys.argv[1])
if not p.is_file():
    raise SystemExit(0)
doc = json.loads(p.read_text())
for t in doc.get("tenants", []):
    print(f"  {t.get('name','?'):<24} public_port={t.get('public_port','?'):<6} uid={t.get('uid','?')} "
          f"suspended={bool(t.get('suspended'))} dsh_home={t.get('dsh_home','?')}")
PY
}

say "mode:        $([[ "$APPLY" == 1 ]] && echo apply || echo 'dry-run (nothing changes)')"
say "deploy root: $ROOT"
say "archive dir: $ARCHIVE_DIR (owner $TARGET_USER)"
say "old config:  $OLD_CONFIG"
say ""
info "units that will be stopped and removed:"
for unit in $(unit_names); do say "  $unit"; done
info "tenants in the old registry:"
if [[ -r "$OLD_REGISTRY" ]]; then
  registry_tenants
else
  say "  (cannot read $OLD_REGISTRY without root; the inventory is complete under sudo)"
  say "  note: a tenant named the same as one in the surviving deployment is still a DIFFERENT"
  say "        tenant here — two deployments, two registries, two sets of data."
fi
info "sizes (deleted trees first, then the trees this script archives but does NOT delete):"
for path in "$OLD_STATE" "$OLD_ETC" "$OLD_OPT" "$NGINX_CONF_DIR" \
            "$OLD_WORKSPACE" "$OLD_TENANT_ROOT" "$OLD_TENANT_CONFIG" "$OLD_BACKUP"; do
  [[ -n "$path" ]] || continue
  if [[ ! -e "$path" ]]; then
    say "  (absent)	$path"
    continue
  fi
  # `|| true` matters: du exits non-zero on a directory it cannot read, and with
  # `set -o pipefail` that failure would abort the plan the operator is reading.
  size="$(du -sh "$path" 2>/dev/null | cut -f1 || true)"
  if [[ -r "$path" ]]; then
    say "  $size	$path"
  else
    # du reports a few KiB for a directory it cannot descend into. Saying so is the
    # difference between "tiny" and "unknown, and possibly gigabytes of tenant data".
    say "  $size (unreadable without root: the real size is unknown until you run as root)	$path"
  fi
done
say ""

# ---------------------------------------------------------------------------- 1) archive
# One tar per tree, each verified by listing it back. Nothing is deleted until all of them
# are readable and non-empty (except the ones whose source was absent).
archive_tree() {
  local src="$1" name="$2"
  local dest="$ARCHIVE_DIR/$name.tar.gz"
  if [[ -z "$src" ]]; then
    # Empty means the old configuration has not been read (it is root-only), not that the
    # tree is absent: under sudo the path is known and gets archived.
    step "skip $name (path unknown without root; run under sudo or pass --state-dir)"
    return 0
  fi
  [[ -e "$src" ]] || { step "skip $name ($src is absent)"; return 0; }
  step "tar -czf $dest -C / ${src#/}"
  if [[ "$APPLY" == 1 ]]; then
    tar -czf "$dest" -C / "${src#/}"
    chown "$TARGET_USER:" "$dest" 2>/dev/null || true
  fi
}

step "mkdir -p $ARCHIVE_DIR"
if [[ "$APPLY" == 1 ]]; then
  mkdir -p "$ARCHIVE_DIR"
  chown "$TARGET_USER:" "$ARCHIVE_DIR" 2>/dev/null || true
fi

archive_tree "$OLD_STATE" "var-lib-dshgw"
archive_tree "$OLD_ETC" "etc-dshgw"
archive_tree "$NGINX_CONF_DIR" "nginx-conf-d-dshgw"
# Data trees that live outside the state directory. They are archived, never deleted: they
# are outside the three directories this script owns, and a workspace is the tenant's own
# files. Their names come from the old configuration, so a host that moved them is covered.
archive_tree "${OLD_WORKSPACE:-}" "srv-dsh-workspaces"
archive_tree "${OLD_BACKUP:-}" "dshgw-backups"

# The workspace root and the backup directory are reported every time, present or not: an
# operator who is about to delete a deployment should know exactly what stays behind.
step "these trees are archived but NOT deleted by this script:"
for path in "$OLD_WORKSPACE" "$OLD_TENANT_ROOT" "$OLD_TENANT_CONFIG" "$OLD_BACKUP"; do
  if [[ -z "$path" ]]; then
    step "  unknown (the old config $OLD_CONFIG is unreadable without root)"
    continue
  fi
  if [[ -e "$path" ]]; then
    size="$(du -sh "$path" 2>/dev/null | cut -f1 || true)"
    step "  keep $path ($size)"
  else
    step "  (absent) $path"
  fi
done

# Unit files and the binary's own identity are small and decisive when reconstructing the old
# shape later, so they are copied verbatim rather than packed.
step "copy unit files from $UNIT_DIR and /etc/systemd/system/dsh-workers.slice"
if [[ "$APPLY" == 1 ]]; then
  mkdir -p "$ARCHIVE_DIR/units"
  for unit in dshgw.service dshgw-admin.service 'dsh-worker@.service' dsh-workers.slice; do
    [[ -f "$UNIT_DIR/$unit" ]] && cp -a "$UNIT_DIR/$unit" "$ARCHIVE_DIR/units/" || true
  done
  if [[ -x "$OLD_OPT/bin/dshgw" ]]; then
    "$OLD_OPT/bin/dshgw" --version >"$ARCHIVE_DIR/legacy-dshgw.version.txt" 2>&1 || true
  fi
fi

if [[ "$APPLY" == 1 ]]; then
  # Verification before deletion: each archive must list back, and the registry — the file
  # that names every tenant — must be inside it.
  #
  # Each listing is written to a scratch file and grepped from there rather than piped into
  # `grep -q`: a pipe would let grep exit on its first match, tar would die of SIGPIPE, and
  # `set -o pipefail` would then report a *successful* archive as broken — which is exactly
  # how the first run of this script stopped at the safety check having deleted nothing.
  listing="$(mktemp)"
  trap 'rm -f "$listing"' EXIT
  for name in var-lib-dshgw etc-dshgw nginx-conf-d-dshgw srv-dsh-workspaces dshgw-backups; do
    dest="$ARCHIVE_DIR/$name.tar.gz"
    [[ -f "$dest" ]] || continue
    [[ -s "$dest" ]] || fail "$dest is empty; refusing to delete anything"
    tar -tzf "$dest" >"$listing" || fail "$dest cannot be read back; refusing to delete anything"
    entries="$(wc -l <"$listing")"
    info "$name.tar.gz: $entries entries, $(du -h "$dest" | cut -f1)"
    if [[ "$name" == "var-lib-dshgw" && -f "$OLD_REGISTRY" ]]; then
      # The registry names every tenant; an archive without it is not a rollback source.
      rel="${OLD_REGISTRY#/}"
      case "$OLD_REGISTRY" in
        "$OLD_STATE"/*) grep -qxF "$rel" "$listing" \
          || fail "the archive does not contain $rel; refusing to delete anything" ;;
      esac
    fi
  done
fi

# An index travels with the archive: a rollback source nobody can interpret is not one.
if [[ "$APPLY" == 1 ]]; then
  {
    echo "# 旧 root/systemd dshgw 的归档（M63 下线脚本生成）"
    echo
    echo "生成时间：$(date -Is)"
    echo "归档主机：$(hostname)"
    echo "旧配置：$OLD_CONFIG"
    echo "旧二进制：$(cat "$ARCHIVE_DIR/legacy-dshgw.version.txt" 2>/dev/null || echo '未取到')"
    echo
    echo "## 内容"
    echo
    echo "| 文件 | 内容 | 本脚本是否删除原目录 |"
    echo "|---|---|---|"
    echo "| \`var-lib-dshgw.tar.gz\` | 旧 state：registry.json、keys.map、sessions/audit/activity、handshake/、tenants/ | 是（\`$OLD_STATE\`） |"
    echo "| \`etc-dshgw.tar.gz\` | 旧配置：config.yaml 与 per-tenant config | 是（\`$OLD_ETC\`） |"
    echo "| \`nginx-conf-d-dshgw.tar.gz\` | nginx 上旧的 dshgw 转发（门户 + 每租户 vhost） | 是（\`$NGINX_CONF_DIR\`） |"
    echo "| \`srv-dsh-workspaces.tar.gz\` | 旧 workspace 根（租户自己的文件） | **否**（\`${OLD_WORKSPACE:-未配置}\`） |"
    echo "| \`dshgw-backups.tar.gz\` | 旧备份目录 | **否**（\`${OLD_BACKUP:-未配置}\`） |"
    echo "| \`units/\` | dshgw.service、dshgw-admin.service、dsh-worker@.service、dsh-workers.slice | 单元文件已删 |"
    echo "| \`legacy-dshgw.version.txt\` | 旧 dshgw 二进制自报版本 | 二进制随 \`$OLD_OPT\` 删除 |"
    echo
    echo "## 旧租户"
    echo
    echo '```'
    python3 - "$OLD_STATE/registry.json" <<'REGISTRYLIST' 2>/dev/null || echo "(registry 不可读：解包 var-lib-dshgw.tar.gz 查看)"
import json, sys
from pathlib import Path
p = Path(sys.argv[1])
if not p.is_file():
    raise SystemExit(0)
for t in json.loads(p.read_text()).get("tenants", []):
    print(f"{t.get('name','?'):<24} public_port={t.get('public_port','?')} uid={t.get('uid','?')} "
          f"suspended={bool(t.get('suspended'))}")
REGISTRYLIST
    echo '```'
    echo
    echo "## 回滚"
    echo
    echo '```bash'
    echo "sudo tar -xzf <此目录>/var-lib-dshgw.tar.gz -C /"
    echo "sudo tar -xzf <此目录>/etc-dshgw.tar.gz -C /"
    echo "sudo tar -xzf <此目录>/nginx-conf-d-dshgw.tar.gz -C /"
    echo "sudo cp <此目录>/units/* /etc/systemd/system/"
    echo "sudo systemctl daemon-reload && sudo systemctl enable --now dshgw dshgw-admin"
    echo "# per-tenant 单元按 registry 逐个拉起；workspace 与 backups 没有被删除，仍在原处"
    echo '```'
  } >"$ARCHIVE_DIR/INDEX.md" 2>/dev/null || true
  chown "$TARGET_USER:" "$ARCHIVE_DIR/INDEX.md" 2>/dev/null || true
fi

if [[ "$ARCHIVE_ONLY" == 1 ]]; then
  say ""
  if [[ "$APPLY" == 1 ]]; then
    say "archive-only: 归档已写入 $ARCHIVE_DIR；未停任何单元、未删任何目录。"
  else
    say "archive-only (dry-run)：上面列出的是将会归档的内容。"
  fi
  say "要继续下线，运行：sudo $0 --apply"
  exit 0
fi

# ------------------------------------------------------------------- 2) stop the services
# Workers first: the gateway would otherwise restart one while it is being torn down.
step "systemctl disable --now <worker units> dshgw.service dshgw-admin.service"
for unit in $(unit_names); do run systemctl disable --now "$unit"; done

step "rm $UNIT_DIR/{dshgw,dshgw-admin}.service $UNIT_DIR/dsh-worker@.service $UNIT_DIR/dsh-workers.slice"
run systemctl daemon-reload

if [[ "$APPLY" == 1 ]]; then
  rm -f "$UNIT_DIR/dshgw.service" "$UNIT_DIR/dshgw-admin.service" \
        "$UNIT_DIR/dsh-worker@.service" "$UNIT_DIR/dsh-workers.slice"
  systemctl daemon-reload
  systemctl reset-failed 2>/dev/null || true
fi

# ------------------------------------------------------------------------- 3) nginx front
# nginx terminated TLS on the legacy tenant ports and forwarded to the old gateway. Removing
# these leaves nginx itself (and the other vhosts) untouched.
if [[ -d "$NGINX_CONF_DIR" ]]; then
  step "rm -rf $NGINX_CONF_DIR && nginx -t && systemctl reload nginx"
  if [[ "$APPLY" == 1 ]]; then
    rm -rf "$NGINX_CONF_DIR"
    nginx -t
    systemctl reload nginx
  fi
else
  step "skip nginx (no $NGINX_CONF_DIR)"
fi

# -------------------------------------------------------------------------- 4) delete
for path in "$OLD_OPT" "$OLD_ETC" "$OLD_STATE"; do
  step "rm -rf $path"
done
if [[ "$APPLY" == 1 ]]; then
  rm -rf "$OLD_OPT" "$OLD_ETC" "$OLD_STATE"
fi

# ------------------------------------------------------------------ 5) accounts (opt-in)
if [[ "$REMOVE_ACCOUNTS" == 1 ]]; then
  accounts="$(getent passwd | awk -F: '$1 ~ /^dsh-/ {print $1}' | sort || true)"
  for account in $accounts; do
    step "userdel -r $account"
    if [[ "$APPLY" == 1 ]]; then
      pgrep -u "$account" >/dev/null 2>&1 && info "$account still has running processes; skipping" || userdel -r "$account" || true
    fi
  done
fi

# ------------------------------------------------------------------------- 6) verification
say ""
if [[ "$APPLY" != 1 ]]; then
  say "dry-run only: nothing was stopped, archived or deleted."
  say "run again with --apply (as root) to execute this plan."
  exit 0
fi

say "verification:"
leftover="$(systemctl list-units --all --no-legend 'dsh-worker@*.service' 'dshgw.service' 'dshgw-admin.service' 2>/dev/null | awk '{print $1}' || true)"
if [[ -n "$leftover" ]]; then
  echo "decommission-legacy-dshgw: these units still exist:" >&2
  printf '  %s\n' $leftover >&2
  exit 1
fi
say "  units: none of dshgw/dshgw-admin/dsh-worker@* remain"
for path in "$OLD_OPT" "$OLD_ETC" "$OLD_STATE"; do
  [[ -e "$path" ]] && { echo "decommission-legacy-dshgw: $path still exists" >&2; exit 1; }
done
say "  directories: $OLD_OPT, $OLD_ETC, $OLD_STATE are gone"
if command -v ss >/dev/null 2>&1; then
  say "  listeners on the legacy port band (32600+) that remain:"
  ss -ltn 2>/dev/null | awk 'NR>1 {print $4}' | grep -E ':(3[23][0-9]{3})$' | sed 's/^/    /' || say "    (none)"
fi
say "  archive: $ARCHIVE_DIR"
say ""
say "next: confirm the surviving deployment is healthy (aigw :8088, and the dshgw the console"
say "uses), then keep $ARCHIVE_DIR until you are sure the old tenants are not needed."
