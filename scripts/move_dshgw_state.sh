#!/usr/bin/env bash
# Move a dshgw state tree to a new root and rewrite the absolute paths it embeds (M63).
#
# Why a script and not a `mv`: dshgw stores absolute paths in more than one place, and the
# ones it validates hardest are the tenant records. Moving the tree without rewriting them
# leaves the gateway pointing at directories that no longer exist — which shows up as a
# tenant profile that cannot be built, or a workspace list full of dead entries.
#
# What is rewritten (and only what dshgw itself manages):
#   * state/registry.json                                     (each tenant's dsh_home/workspace)
#   * state/tenants/*/.dsh/profiles/web/cordis.patch.yml       (the picker's root)
#   * state/tenants/*/.dsh/storages/workspace.json             (the workspace list)
#   * state/tenants/*/.dsh/storages/session_projcache/sessions/*.json
# What is NEVER rewritten:
#   * state/workspaces/**  — that is the tenants' own content (it may contain a checkout of
#     this repository, whose files merely mention the old path).
#   * template-home/**     — a prepared profile, rebuilt by deploy/dshgw/prepare-template.sh.
#
# Usage:
#   scripts/move_dshgw_state.sh --from <old-root> --to <new-root> [--apply] [--force]
#
#   --from   the directory that holds the state tree today (its parent for a state_dir, or
#            the state_dir itself; both are accepted)
#   --to     the directory that will hold it (usually <deploy root>/data/dshgw-verify)
#   --apply  actually move and rewrite; without it nothing changes and every step is printed
#   --force  proceed even though a dshgw process looks like it is running
set -euo pipefail

FROM=""
TO=""
APPLY=0
FORCE=0

usage() { sed -n '2,28p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --from) FROM="$2"; shift 2 ;;
    --to) TO="$2"; shift 2 ;;
    --apply) APPLY=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$FROM" && -n "$TO" ]] || { usage >&2; exit 2; }

say()  { printf '%s\n' "$*"; }
step() { printf '%s  %s\n' "$([[ "$APPLY" == 1 ]] && echo RUN || echo PLAN)" "$*"; }
fail() { echo "move-dshgw-state: $*" >&2; exit 1; }

abs() { (cd "$(dirname "$1")" 2>/dev/null && printf '%s/%s\n' "$(pwd)" "$(basename "$1")") || printf '%s\n' "$1"; }

FROM="$(abs "$FROM")"
TO="$(abs "$TO")"

# The state tree is the one that holds registry.json; accept both the parent and the
# state_dir itself so the caller does not have to know which one they mean.
if [[ -f "$FROM/registry.json" ]]; then
  OLD_STATE="$FROM"
elif [[ -f "$FROM/state/registry.json" ]]; then
  OLD_STATE="$FROM/state"
  [[ -d "$FROM/template-home" || -f "$FROM/dshgw.yaml" ]] || fail "$FROM holds state/ but looks like neither a state root nor a deployment root"
  FROM="$FROM/state"
  # Everything that travels with the state lives beside it in the deployment root.
  EXTRA_FROM="$(dirname "$FROM")"
else
  fail "$FROM holds no registry.json (looked in it and in its state/ subdirectory)"
fi
NEW_STATE="$TO/state"

[[ -d "$OLD_STATE" ]] || fail "no state directory at $OLD_STATE"
[[ "$OLD_STATE" != "$NEW_STATE" ]] || fail "--from and --to resolve to the same state directory"
if [[ -e "$NEW_STATE" ]]; then
  fail "$NEW_STATE already exists; move it aside first (a half-moved tree is worse than none)"
fi

# Refuse to move a tree out from under a running gateway: its workers hold binds and its
# admin socket would keep answering at a path that no longer exists. The check is anchored
# on THIS state tree's prefix and on the process *name* (pgrep -x), so it neither matches
# this script's own command line nor a second, unrelated deployment on the same host.
if [[ "$FORCE" != 1 ]]; then
  busy=""
  for pid in $(pgrep -x dshgw 2>/dev/null || true); do
    cmd="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null || true)"
    case "$cmd" in
      *"$(dirname "$OLD_STATE")"*) busy="$busy  pid $pid: $cmd"$'\n' ;;
    esac
  done
  if [[ -n "$busy" ]]; then
    echo "move-dshgw-state: this state tree is in use:" >&2
    printf '%s' "$busy" >&2
    fail "stop that process first (systemctl --user stop <unit>) or pass --force"
  fi
fi

say "mode:        $([[ "$APPLY" == 1 ]] && echo apply || echo 'dry-run (nothing changes)')"
say "old state:   $OLD_STATE"
say "new state:   $NEW_STATE"
say "old prefix:  ${EXTRA_FROM:-$(dirname "$OLD_STATE")}"
say "new prefix:  $(dirname "$NEW_STATE")"
say ""

# 1) Move the tree. mkdir -p first: the destination's parent may be the deployment's data
#    root, which need not exist yet on a fresh checkout.
step "mkdir -p $(dirname "$NEW_STATE")"
step "mv $OLD_STATE $NEW_STATE"
if [[ -n "${EXTRA_FROM:-}" ]]; then
  for entry in template-home current; do
    if [[ -e "$EXTRA_FROM/$entry" && ! -e "$TO/$entry" ]]; then
      step "mv $EXTRA_FROM/$entry $TO/$entry"
    fi
  done
  for entry in dshgw.log gwproxy.log; do
    if [[ -f "$EXTRA_FROM/$entry" && ! -e "$TO/$entry" ]]; then
      step "mv $EXTRA_FROM/$entry $TO/$entry"
    fi
  done
fi

if [[ "$APPLY" == 1 ]]; then
  mkdir -p "$(dirname "$NEW_STATE")"
  mv "$OLD_STATE" "$NEW_STATE"
  if [[ -n "${EXTRA_FROM:-}" ]]; then
    for entry in template-home current; do
      if [[ -e "$EXTRA_FROM/$entry" && ! -e "$TO/$entry" ]]; then
        mv "$EXTRA_FROM/$entry" "$TO/$entry"
      fi
    done
    for entry in dshgw.log gwproxy.log; do
      if [[ -f "$EXTRA_FROM/$entry" && ! -e "$TO/$entry" ]]; then
        mv "$EXTRA_FROM/$entry" "$TO/$entry"
      fi
    done
  fi
fi

# 2) Rewrite the embedded absolute paths, in the new location.
OLD_PREFIX="$(dirname "$OLD_STATE")"
NEW_PREFIX="$(dirname "$NEW_STATE")"
# A dry run must still show WHAT would be rewritten, so it inspects the tree where it
# currently is; an apply inspects it where it now is. The file set is identical either way.
if [[ "$APPLY" == 1 ]]; then REWRITE_ROOT="$NEW_STATE"; else REWRITE_ROOT="$OLD_STATE"; fi
step "rewrite $OLD_PREFIX -> $NEW_PREFIX inside registry.json and the per-tenant artifacts"
python3 - "$REWRITE_ROOT" "$OLD_PREFIX" "$NEW_PREFIX" "$APPLY" <<'PY'
import sys
from pathlib import Path

state = Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
apply = sys.argv[4] == "1"

# Deterministic order and an explicit list: "every file under the tree" would rewrite the
# tenants' own workspaces, which is their content, not the gateway's bookkeeping.
targets = []
registry = state / "registry.json"
if registry.exists():
    targets.append(registry)
for tenant in sorted((state / "tenants").glob("*")) if (state / "tenants").is_dir() else []:
    for rel in ("profiles/web/cordis.patch.yml", "storages/workspace.json"):
        candidate = tenant / ".dsh" / rel
        if candidate.is_file():
            targets.append(candidate)
    cache = tenant / ".dsh" / "storages" / "session_projcache" / "sessions"
    if cache.is_dir():
        targets.extend(sorted(p for p in cache.glob("*.json") if p.is_file()))

changed = 0
for path in targets:
    try:
        text = path.read_text(encoding="utf-8")
    except (UnicodeDecodeError, OSError):
        print(f"SKIP  {path} (not readable as UTF-8 text)")
        continue
    if old not in text:
        continue
    count = text.count(old)
    print(f"{'REWRITE' if apply else 'PLAN'}  {path} ({count} occurrence(s))")
    changed += 1
    if apply:
        path.write_text(text.replace(old, new), encoding="utf-8")

print(f"{'rewrote' if apply else 'would rewrite'} {changed} file(s) of {len(targets)} inspected")
PY

# 3) Prove nothing but the tenants' own content still names the old prefix.
step "check that no gateway-managed file still contains $OLD_PREFIX"
if [[ "$APPLY" == 1 ]]; then
  stale="$(grep -rl --binary-files=without-match -- "$OLD_PREFIX" "$NEW_STATE" 2>/dev/null \
    | grep -v "/workspaces/" | grep -v "/template-home/" || true)"
  if [[ -n "$stale" ]]; then
    echo "move-dshgw-state: these files still contain the old prefix:" >&2
    printf '  %s\n' $stale >&2
    echo "move-dshgw-state: the tree is moved; re-run this script's rewrite step after checking them" >&2
    exit 1
  fi
  say "clean: only workspaces/ and template-home/ may legitimately mention the old prefix"
else
  say "(dry-run: the check runs after --apply)"
fi

say ""
say "next steps:"
say "  1. point the configuration at the new state root and restart the service"
say "  2. for each tenant: curl -H 'Host: <public_host>:<public_port>' http://127.0.0.1:<public_port>/api → 401"
say "  3. only then remove the old directory if it is empty"
