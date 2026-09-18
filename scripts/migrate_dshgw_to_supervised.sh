#!/usr/bin/env bash
# Migrate a root/systemd dshgw deployment to the aigw-supervised rootless shape.
#
# What changes: tenants stop being systemd units running as dsh-<tenant> accounts
# and become bubblewrap child processes of dshgw, which is itself a child of aigw
# running as one unprivileged account. Tenant data does not move: this script
# repoints the new shape at the directories the old one already populated.
#
# The script is deliberately conservative:
#   * --dry-run (default) prints every privileged action and changes nothing;
#   * --apply backs up the registry, stops the old units, fixes ownership, marks
#     every tenant as bwrap, appends a dshgw: block to the aigw config, and then
#     waits for tenants to answer their /api probe again;
#   * --rollback re-enables the old units and restores the registry backup.
#
# The old units are disabled but NOT deleted: rollback must stay possible until you
# decide the migration held. Only after that should you remove
# /opt/dshgw, /etc/dshgw and the dsh-* accounts (documented in TODO).
#
# Usage:
#   scripts/migrate_dshgw_to_supervised.sh [--dry-run|--apply|--rollback] \
#     [--aigw-user USER] [--aigw-config PATH] [--old-config PATH] \
#     [--old-registry PATH] [--state-dir PATH] [--backup-dir PATH] \
#     [--aigw-binary PATH] [--dshgw-binary PATH] [--skip-start]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="dry-run"
AIGW_USER="${SUDO_USER:-$(id -un)}"
AIGW_CONFIG="$ROOT/config.yaml"
OLD_CONFIG="/etc/dshgw/config.yaml"
OLD_REGISTRY="/var/lib/dshgw/registry.json"
STATE_DIR=""
BACKUP_DIR=""
AIGW_BINARY="$ROOT/bin/aigw"
DSHGW_BINARY="$ROOT/bin/dshgw"
SKIP_START=0
PROBE_TIMEOUT=60
# --dry-run can be combined with --apply/--rollback to print that mode's plan
# without changing anything (and without needing root).
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --apply) MODE="apply"; shift ;;
    --rollback) MODE="rollback"; shift ;;
    --aigw-user) AIGW_USER="$2"; shift 2 ;;
    --aigw-config) AIGW_CONFIG="$2"; shift 2 ;;
    --old-config) OLD_CONFIG="$2"; shift 2 ;;
    --old-registry) OLD_REGISTRY="$2"; shift 2 ;;
    --state-dir) STATE_DIR="$2"; shift 2 ;;
    --backup-dir) BACKUP_DIR="$2"; shift 2 ;;
    --aigw-binary) AIGW_BINARY="$2"; shift 2 ;;
    --dshgw-binary) DSHGW_BINARY="$2"; shift 2 ;;
    --skip-start) SKIP_START=1; shift ;;
    -h|--help) sed -n '2,24p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

say()  { printf '%s\n' "$*"; }
plan() { printf 'PLAN  %s\n' "$*"; }
run()  {
  # Executes in apply and rollback; plans in dry-run. (Rollback used to be planned
  # but never executed, which is a "rollback" that does nothing.)
  if [[ "$DRY_RUN" == 0 && "$MODE" != "dry-run" ]]; then
    printf 'RUN   %s\n' "$*"
    "$@"
  else
    plan "$*"
  fi
}

fail() { echo "migrate: $*" >&2; exit 1; }

# ---------------------------------------------------------------- old deployment
# The old deployment is described by its own config: reading it is how this script
# learns where the tenants, workspaces and registry actually live, instead of
# assuming the defaults.
read_old_config() {
  [[ -f "$OLD_CONFIG" ]] || fail "old config not found: $OLD_CONFIG (pass --old-config)"
  python3 - "$OLD_CONFIG" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1])) or {}
deploy = doc.get("deploy") or {}
dsh = doc.get("dsh") or {}
out = {
    "state_dir": doc.get("state_dir") or "",
    "tenant_root": doc.get("tenant_root") or "",
    "workspace_root": doc.get("workspace_root") or "",
    "tenant_config_root": deploy.get("tenant_config_root") or "",
    "backup_dir": deploy.get("backup_dir") or "",
    "worker_unit": deploy.get("worker_unit") or "dsh-worker@.service",
    "gateway_unit": deploy.get("gateway_unit") or "dshgw.service",
    "node_bin": dsh.get("node_bin") or "",
    "bin_js": dsh.get("bin_js") or "",
    "current_link": dsh.get("current_link") or "",
    "registry_path": doc.get("registry_path") or "",
}
for key, value in out.items():
    print(f"{key.upper()}={value}")
PY
}

OLD_UNITS="$(
  systemctl list-units --all --no-legend 'dsh-worker@*.service' 2>/dev/null | awk '{print $1}' || true
)"

# ---------------------------------------------------------------- plan / apply
main() {
  # shellcheck disable=SC1090
  while IFS='=' read -r key value; do
    case "$key" in
      STATE_DIR) OLD_STATE="$value" ;;
      TENANT_ROOT) OLD_TENANT_ROOT="$value" ;;
      WORKSPACE_ROOT) OLD_WORKSPACE_ROOT="$value" ;;
      TENANT_CONFIG_ROOT) OLD_TENANT_CONFIG_ROOT="$value" ;;
      BACKUP_DIR) OLD_BACKUP_DIR="$value" ;;
      WORKER_UNIT) OLD_WORKER_UNIT="$value" ;;
      GATEWAY_UNIT) OLD_GATEWAY_UNIT="$value" ;;
      NODE_BIN) OLD_NODE_BIN="$value" ;;
      BIN_JS) OLD_BIN_JS="$value" ;;
      CURRENT_LINK) OLD_CURRENT_LINK="$value" ;;
      REGISTRY_PATH) OLD_REGISTRY_PATH="$value" ;;
    esac
  done < <(read_old_config)

  [[ -n "${OLD_STATE:-}" ]] || OLD_STATE="/var/lib/dshgw"
  OLD_REGISTRY_PATH="${OLD_REGISTRY_PATH:-$OLD_REGISTRY}"
  [[ -n "${STATE_DIR:-}" ]] || STATE_DIR="$OLD_STATE"
  [[ -n "${BACKUP_DIR:-}" ]] || BACKUP_DIR="${OLD_BACKUP_DIR:-$HOME/dshgw-migration-backup}"

  say "mode: $MODE$([[ "$DRY_RUN" == 1 ]] && echo " (dry-run)" || true)"
  say "aigw user:       $AIGW_USER"
  say "old state:       $OLD_STATE (registry $OLD_REGISTRY_PATH)"
  say "old tenant root: ${OLD_TENANT_ROOT:-<derived>}"
  say "old workspace:   ${OLD_WORKSPACE_ROOT:-<derived>}"
  say "new state dir:   $STATE_DIR"
  say "backup dir:      $BACKUP_DIR"

  if [[ "$MODE" == "rollback" ]]; then
    rollback
    return
  fi

  # ------------------------------------------------------------- preconditions
  if [[ "$MODE" == "apply" && "$DRY_RUN" == 0 ]]; then
    [[ "$(id -u)" == "0" ]] || fail "--apply changes ownership and systemd units: run it as root"
    id -u "$AIGW_USER" >/dev/null 2>&1 || fail "no such account: $AIGW_USER"
    [[ -x "$AIGW_BINARY" && -x "$DSHGW_BINARY" ]] || fail "build first: make build dshgw-build"
    [[ "$(dirname "$AIGW_BINARY")" == "$(dirname "$DSHGW_BINARY")" ]] || fail "aigw and dshgw must sit in the same directory"
  fi
  if [[ -f "$OLD_REGISTRY_PATH" ]]; then
    tenant_count=$(python3 -c "import json,sys;print(len(json.load(open(sys.argv[1]))['tenants']))" "$OLD_REGISTRY_PATH")
  else
    tenant_count=0
  fi
  say "tenants in registry: $tenant_count"
  say "old worker units:    ${OLD_UNITS//$'\n'/ }"

  # --------------------------------------------------------------- backup first
  stamp="$(date -u +%Y%m%d-%H%M%S)"
  run install -d -m 0700 "$BACKUP_DIR"
  if [[ -f "$OLD_REGISTRY_PATH" ]]; then
    run cp -a "$OLD_REGISTRY_PATH" "$BACKUP_DIR/registry.json.$stamp"
  fi
  if [[ -n "${OLD_TENANT_CONFIG_ROOT:-}" && -d "$OLD_TENANT_CONFIG_ROOT" ]]; then
    run tar -C "$(dirname "$OLD_TENANT_CONFIG_ROOT")" -czf "$BACKUP_DIR/tenant-config.$stamp.tar.gz" "$(basename "$OLD_TENANT_CONFIG_ROOT")"
  fi

  # --------------------------------------------------------- stop the old shape
  for unit in $OLD_UNITS; do
    run systemctl disable --now "$unit"
  done
  run systemctl disable --now "${OLD_GATEWAY_UNIT:-dshgw.service}"
  run systemctl disable --now dshgw-admin.service

  # ------------------------------------------------- ownership for the new shape
  # Every worker now runs as the aigw account, so that account must own the state
  # the tenants already have. This is the one irreversible-ish step; the backup
  # above is the safety net (ownership is recorded by tar in the archive).
  for path in "$STATE_DIR" "${OLD_WORKSPACE_ROOT:-}" "${OLD_TENANT_CONFIG_ROOT:-}"; do
    [[ -n "$path" && -e "$path" ]] || continue
    run chown -R "$AIGW_USER":"$AIGW_USER" "$path"
    run chmod -R go-rwx "$path"
  done

  # ------------------------------------------------------------ registry update
  # The new shape has exactly one mode (bwrap) and one worker account, so the
  # recorded per-tenant identity becomes the aigw account.
  run python3 - "$OLD_REGISTRY_PATH" "$AIGW_USER" <<'PY'
import json, os, pwd, sys
path, user = sys.argv[1], sys.argv[2]
uid = pwd.getpwnam(user).pw_uid
doc = json.load(open(path))
for tenant in doc.get("tenants", []):
    tenant["isolation"] = "bwrap"
    tenant["uid"] = uid
    tenant.setdefault("suspended", False)
with open(path, "w") as handle:
    json.dump(doc, handle, indent=2)
    handle.write("\n")
os.chmod(path, 0o600)
print(f"registry updated: {len(doc.get('tenants', []))} tenant(s) marked bwrap/uid {uid}")
PY
  run chown "$AIGW_USER":"$AIGW_USER" "$OLD_REGISTRY_PATH"

  # ------------------------------------------------------- aigw configuration
  if grep -qE '^dshgw:' "$AIGW_CONFIG" 2>/dev/null; then
    say "NOTE  $AIGW_CONFIG already has a dshgw: block; verify it matches the snippet below"
  else
    snippet="$BACKUP_DIR/dshgw-block.$stamp.yaml"
    cat >"$snippet" <<SNIPPET
dshgw:
  enabled: true
  state_dir: $STATE_DIR
  tenant_root: ${OLD_TENANT_ROOT:-$STATE_DIR/tenants}
  workspace_root: ${OLD_WORKSPACE_ROOT:-$STATE_DIR/workspaces}
  node_bin: ${OLD_NODE_BIN:-/opt/dsh/node/bin/node}
  current_link: ${OLD_CURRENT_LINK:-/opt/dsh/current}
  template_home: /opt/dshgw/share/template-home
  plugin_path: /opt/dshgw/share/dsh-plugin/picker-clamp.js
  public_listen: 127.0.0.1
SNIPPET
    say "wrote $snippet"
    if [[ "$MODE" == "apply" && "$DRY_RUN" == 0 ]]; then
      cat "$snippet" >>"$AIGW_CONFIG"
      say "appended the dshgw: block to $AIGW_CONFIG"
    else
      plan "append $snippet to $AIGW_CONFIG"
    fi
  fi

  # ------------------------------------------------------------------- start it
  if [[ "$SKIP_START" == 0 ]]; then
    run runuser -u "$AIGW_USER" -- env "XDG_RUNTIME_DIR=/run/user/$(id -u "$AIGW_USER")" \
      bash -c "cd '$ROOT' && scripts/aigw_user_service.sh install --config '$AIGW_CONFIG' --bin '$AIGW_BINARY'"
  fi

  # ------------------------------------------------------------------- verify it
  if [[ "$MODE" == "apply" && "$DRY_RUN" == 0 && "$SKIP_START" == 0 ]]; then
    say "waiting for tenants to answer their /api probe ..."
    python3 - "$OLD_REGISTRY_PATH" "$PROBE_TIMEOUT" <<'PY'
import json, socket, sys, time
doc = json.load(open(sys.argv[1]))
deadline, failures = time.time() + float(sys.argv[2]), []
for tenant in doc.get("tenants", []):
    if tenant.get("suspended"):
        continue
    port, ok = tenant["worker_port"], False
    while time.time() < deadline and not ok:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=2) as conn:
                conn.sendall(f"GET /api HTTP/1.0\r\nHost: 127.0.0.1:{port}\r\n\r\n".encode())
                ok = b" 401" in conn.recv(64)
        except OSError:
            time.sleep(1)
    print(f"{'OK  ' if ok else 'FAIL'} {tenant['name']} (worker port {port})")
    if not ok:
        failures.append(tenant["name"])
sys.exit(1 if failures else 0)
PY
  else
    plan "probe every tenant's /api for 401"
  fi

  say ""
  say "next:"
  say "  1) confirm tenants answer on their public ports through dshgw's edge"
  say "  2) keep $BACKUP_DIR until you are satisfied, then delete the old units and the dsh-* accounts"
  say "  3) rollback if needed: $0 --rollback --old-registry $OLD_REGISTRY_PATH --aigw-user $AIGW_USER"
}

rollback() {
  [[ "$DRY_RUN" == 1 || "$(id -u)" == "0" ]] || fail "--rollback re-enables system units: run it as root"
  say "rollback plan: stop the supervised instance, restore the old units and registry"
  run runuser -u "$AIGW_USER" -- env "XDG_RUNTIME_DIR=/run/user/$(id -u "$AIGW_USER")" \
    systemctl --user disable --now aigw-local.service
  for unit in $OLD_UNITS; do
    run systemctl enable --now "$unit"
  done
  run systemctl enable --now "${OLD_GATEWAY_UNIT:-dshgw.service}"
  latest="$(ls -t "$BACKUP_DIR"/registry.json.* 2>/dev/null | head -1 || true)"
  if [[ -n "$latest" ]]; then
    run cp -a "$latest" "$OLD_REGISTRY_PATH"
    run chown root:dshgw "$OLD_REGISTRY_PATH" 2>/dev/null || true
    say "restored registry from $latest"
  else
    say "WARNING no registry backup found in $BACKUP_DIR; restore it by hand"
  fi
  # Ownership is the part of this migration that the old units depend on: the old
  # gateway runs as `dshgw` and every old worker as `dsh-<tenant>`, so restoring the
  # units without restoring ownership would leave them unable to read their own
  # state. The mapping is derived from the (just restored) registry, not guessed.
  target="${latest:-$OLD_REGISTRY_PATH}"
  if [[ -f "$target" ]]; then
    while read -r name dsh_home workspace; do
      run chown -R "dsh-$name":"dsh-$name" "$dsh_home" "$workspace"
    done < <(python3 - "$target" <<'TENANTLIST'
import json, sys
for tenant in json.load(open(sys.argv[1])).get("tenants", []):
    print(tenant["name"], tenant["dsh_home"], tenant["workspace"])
TENANTLIST
)
  fi
  run chown -R root:dshgw "${STATE_DIR:-/var/lib/dshgw}"
  say "the dshgw: block appended to $AIGW_CONFIG is inert while aigw runs with the old binary; remove it when you are done"
}

main "$@"
