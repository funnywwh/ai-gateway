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
#   sudo scripts/decommission_legacy_dshgw.sh [--apply] [--archive-dir DIR]
#                                            [--remove-accounts] [--old-config PATH]
#
#   (no --apply)     print the plan: inventory, what gets archived, what gets stopped
#   --apply          do it (needs root; nothing is deleted before the archive is verified)
#   --archive-dir    where the archive lands (default <repo>/data/prev/legacy-dshgw)
#   --remove-accounts
#                    also delete the per-tenant OS accounts (dsh-<tenant>) and their homes.
#                    Off by default: deleting accounts is the least reversible step here, and
#                    a leftover account is harmless once nothing runs as it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APPLY=0
REMOVE_ACCOUNTS=0
ARCHIVE_DIR=""
OLD_CONFIG="/etc/dshgw/config.yaml"
OLD_STATE=""
OLD_REGISTRY=""
OLD_ETC="/etc/dshgw"
OLD_OPT="/opt/dshgw"
NGINX_CONF_DIR=""
UNIT_DIR="/etc/systemd/system"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --archive-dir) ARCHIVE_DIR="$2"; shift 2 ;;
    --remove-accounts) REMOVE_ACCOUNTS=1; shift ;;
    --old-config) OLD_CONFIG="$2"; shift 2 ;;
    --state-dir) OLD_STATE="$2"; shift 2 ;;
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

if [[ "$APPLY" == 1 && "$(id -u)" != 0 ]]; then
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
out = {
    "state_dir": doc.get("state_dir") or "",
    "registry_path": doc.get("registry_path") or "",
    "nginx_dir": doc.get("nginx_include_path") or doc.get("nginx_dir") or "",
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
info "sizes:"
for path in "$OLD_STATE" "$OLD_ETC" "$OLD_OPT" "$NGINX_CONF_DIR"; do
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
  [[ -e "$src" ]] || { step "skip $src (absent)"; return 0; }
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
  for name in var-lib-dshgw etc-dshgw nginx-conf-d-dshgw; do
    dest="$ARCHIVE_DIR/$name.tar.gz"
    [[ -f "$dest" ]] || continue
    [[ -s "$dest" ]] || fail "$dest is empty; refusing to delete anything"
    tar -tzf "$dest" >/dev/null || fail "$dest cannot be read back; refusing to delete anything"
    info "$name.tar.gz: $(tar -tzf "$dest" | wc -l) entries, $(du -h "$dest" | cut -f1)"
  done
  if [[ -f "$OLD_REGISTRY" ]]; then
    # The registry names every tenant; an archive without it is not a rollback source.
    rel="${OLD_REGISTRY#/}"
    case "$OLD_REGISTRY" in
      "$OLD_STATE"/*) tar -tzf "$ARCHIVE_DIR/var-lib-dshgw.tar.gz" | grep -qF "$rel" \
        || fail "the archive does not contain $rel; refusing to delete anything" ;;
    esac
  fi
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
