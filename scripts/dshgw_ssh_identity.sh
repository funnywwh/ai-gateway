#!/bin/sh
# Reclaim the tenant ssh identities that were copied from an operator key, and provision the
# per-account keys that replace them.
#
# Why this exists: ssh_workspaces.identity_source copied ONE operator key into every account
# that had none. In this deployment it was pointed at the deployment account's own
# ~/.ssh/id_rsa, so eight accounts held a byte-identical copy of the operator's personal key —
# and that key is authorised on the gateway host itself, which turns "a tenant may read its own
# key" into a full escape from the tenant sandbox into the deployment account. The
# configuration key is gone (dshgw refuses to start with it); this script repairs the copies
# that were already handed out and installs one key per account instead.
#
#   scripts/dshgw_ssh_identity.sh purge-shared   [--state-root DIR] [--revoked-key FILE|--revoked-sha256 HEX] [--apply]
#   scripts/dshgw_ssh_identity.sh provision      --tenant NAME --key FILE [--identity-dir DIR] [--state-root DIR] [--apply]
#
# Nothing is written unless --apply is given: the default is a plan you can read first. purge-shared
# writes an `identity-managed` marker next to the key it deletes, which is what stops the running
# gateway from copying the operator key back in; remove that marker for an account you want the
# gateway to seed from identity_dir/<account> again.
set -eu

state_root=data/dshgw-verify
revoked_key=${HOME}/.ssh/id_rsa
revoked_sha=
revoked_fp=
identity_dir=data/dshgw-verify/ssh-keys
seed_dir=data/dshgw-verify/ssh-configs
config=./dshgw.yaml
tenant=
key_file=
apply=0
allow_mounted=0
restart=0
force=0

usage() {
  cat <<'EOF'
usage: dshgw_ssh_identity.sh purge-shared   [--state-root DIR] [--revoked-key FILE|--revoked-sha256 HEX]
                                            [--apply] [--allow-mounted]
       dshgw_ssh_identity.sh provision      --tenant NAME --key FILE [--identity-dir DIR]
                                            [--state-root DIR] [--config PATH] [--apply] [--force] [--restart]
       dshgw_ssh_identity.sh trim-seeds     [--seed-dir DIR] [--state-root DIR] [--apply]

  purge-shared  delete every <workspace>/.ssh/id_rsa that is a copy of the revoked key and
                write the account's identity-managed marker (default: plan only)
  provision     install one account's own key as <identity_dir>/<NAME>, drop that account's
                marker and seed the account's own copy <workspace>/.ssh/id_rsa with the same
                bytes (default: plan only)
  trim-seeds    drop the operator's own host inventory from every alias seed and from the
                accounts that already have an alias list, keeping only the aliases an account
                added itself; snapshots both before rewriting (default: plan only)

  --state-root DIR   deployment state root (default: data/dshgw-verify)
  --revoked-key FILE  key whose copies must go (default: ~/.ssh/id_rsa)
  --revoked-sha256 HEX  match by digest instead, for use after that key file was replaced
  --identity-dir DIR  per-account key directory (default: data/dshgw-verify/ssh-keys)
  --seed-dir DIR      per-account alias seeds (default: data/dshgw-verify/ssh-configs)
  --config PATH       dshgw config the restart command uses (default: ./dshgw.yaml)
  --apply             actually change things
  --allow-mounted     purge while ssh-mounts.json still records mounts
  --force             provision: replace a key the account already holds
  --restart           provision: also restart the account's worker (interrupts its session)
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    purge-shared|provision|trim-seeds) command=$1; shift ;;
    --state-root) state_root=$2; shift 2 ;;
    --revoked-key) revoked_key=$2; shift 2 ;;
    --revoked-sha256) revoked_sha=$2; shift 2 ;;
    --identity-dir) identity_dir=$2; shift 2 ;;
    --seed-dir) seed_dir=$2; shift 2 ;;
    --config) config=$2; shift 2 ;;
    --tenant) tenant=$2; shift 2 ;;
    --key) key_file=$2; shift 2 ;;
    --apply) apply=1; shift ;;
    --allow-mounted) allow_mounted=1; shift ;;
    --restart) restart=1; shift ;;
    --force) force=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
command=${command:-}

[ -n "$command" ] || { usage >&2; exit 2; }

# digest prints the sha256 of a file, or nothing when it cannot be read.
digest() {
  [ -f "$1" ] || return 0
  sha256sum "$1" 2>/dev/null | cut -d' ' -f1
}

# fingerprint prints the public fingerprint of a private key, or nothing when ssh-keygen cannot
# read it. It is what identifies a key across its harmless variants: the same keypair written
# with and without a trailing newline has two digests and one fingerprint, and a copy that
# differs by a byte is still the same key.
fingerprint() {
  ssh-keygen -lf "$1" 2>/dev/null | awk '{print $2}'
}

# same_identity prints a match when the candidate file holds the revoked keypair, whatever its
# bytes look like. The fingerprint is only known when the revoked key file is still readable;
# matching by digest alone would miss a copy that differs by a newline, which is exactly how one
# per-host copy in this deployment survived the first pass.
same_identity() {
  [ -n "$revoked_fp" ] || return 0
  candidate_fp=$(fingerprint "$1")
  [ -n "$candidate_fp" ] && [ "$candidate_fp" = "$revoked_fp" ] && printf 'match\n'
  return 0
}

# workspaces prints one account workspace path per line: the registry is the authority on which
# accounts exist (it is what the gateway itself reads), and a plain directory listing is only a
# fallback for a state root whose registry is missing.
workspaces() {
  registry=$state_root/state/registry.json
  if [ -f "$registry" ]; then
    python3 - "$registry" <<'PY'
import json, sys
with open(sys.argv[1]) as handle:
    document = json.load(handle)
for tenant in document.get("tenants", []):
    workspace = tenant.get("workspace", "")
    if workspace:
        print(workspace)
PY
    return 0
  fi
  for workspace in "$state_root"/state/workspaces/*; do
    [ -d "$workspace" ] && printf '%s\n' "$workspace"
  done
}

account_of() {
  basename "$1"
}

purge_shared() {
  [ -n "$revoked_sha" ] || revoked_sha=$(digest "$revoked_key")
  [ -n "$revoked_fp" ] || revoked_fp=$(fingerprint "$revoked_key")
  if [ -z "$revoked_sha" ] && [ -z "$revoked_fp" ]; then
    echo "purge-shared: cannot read $revoked_key; pass --revoked-key or --revoked-sha256" >&2
    exit 1
  fi
  echo "revoked identity sha256 $revoked_sha${revoked_fp:+ fingerprint $revoked_fp}"

  mounts=$state_root/state/ssh-mounts.json
  if [ "$allow_mounted" -eq 0 ] && [ -f "$mounts" ] && python3 -c '
import json, sys
try:
    document = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
sys.exit(0 if document.get("mounts") else 1)
' "$mounts"; then
    echo "purge-shared: $mounts still records mounts; unmount them first (or pass --allow-mounted)" >&2
    exit 1
  fi

  found=0
  purged=0
  # Through a list file rather than command substitution: a workspace path may contain a space,
  # and a piped loop would run the counters in a subshell.
  list=$(mktemp)
  workspaces > "$list"
  while read -r workspace; do
    [ -n "$workspace" ] || continue
    account=$(account_of "$workspace")
    marker=$workspace/.ssh/identity-managed
    # Every place this account may hold a private key: the account default, and one per host
    # (<.ssh/host_keys/<SHA256(host)>/id_rsa>, written when a host was added with its own key).
    # A per-host copy is the same exposure — and it is easy to miss, because it is not the path
    # the configuration key named.
    candidates=
    [ -f "$workspace/.ssh/id_rsa" ] && candidates="$workspace/.ssh/id_rsa"
    for host_key in "$workspace"/.ssh/host_keys/*/id_rsa; do
      [ -f "$host_key" ] || continue
      candidates="$candidates $host_key"
    done
    if [ -z "$candidates" ]; then
      echo "skip    $account: no private key under $workspace/.ssh"
      continue
    fi
    for candidate in $candidates; do
      found=$((found + 1))
      if [ "$(digest "$candidate")" != "$revoked_sha" ] && [ -z "$(same_identity "$candidate")" ]; then
        echo "keep    $account: $candidate is not the revoked key"
        continue
      fi
      if [ "$apply" -eq 0 ]; then
        echo "would   $account: delete $candidate"
        purged=$((purged + 1))
        continue
      fi
      rm -f "$candidate"
      if [ "$candidate" = "$workspace/.ssh/id_rsa" ]; then
        (umask 077; printf 'managed\n' > "$marker")
        echo "purged  $account: deleted $candidate, wrote $marker"
      else
        echo "purged  $account: deleted $candidate (per-host key)"
      fi
      purged=$((purged + 1))
    done
  done < "$list"
  rm -f "$list"

  if [ "$apply" -eq 0 ]; then
    echo "plan: $purged of $found keys are the revoked identity (dry run; pass --apply)"
  else
    echo "done: $purged of $found keys reclaimed"
  fi
}

provision() {
  [ -n "$tenant" ] || { echo "provision: --tenant is required" >&2; exit 2; }
  [ -n "$key_file" ] || { echo "provision: --key is required" >&2; exit 2; }
  [ -f "$key_file" ] || { echo "provision: $key_file does not exist" >&2; exit 1; }

  new_sha=$(digest "$key_file")
  [ -n "$revoked_sha" ] || revoked_sha=$(digest "$revoked_key")
  if [ -n "$revoked_sha" ] && [ "$new_sha" = "$revoked_sha" ]; then
    echo "provision: $key_file IS the revoked key; issuing a copy of it is the incident this repairs" >&2
    exit 1
  fi
  for existing in "$identity_dir"/*; do
    [ -f "$existing" ] || continue
    [ "$(basename "$existing")" = "$tenant" ] && continue
    if [ "$(digest "$existing")" = "$new_sha" ]; then
      echo "provision: $key_file is the same key as $existing; one key per account" >&2
      exit 1
    fi
  done

  workspace=
  list=$(mktemp)
  workspaces > "$list"
  while read -r candidate; do
    [ -n "$candidate" ] || continue
    if [ "$(account_of "$candidate")" = "$tenant" ]; then
      workspace=$candidate
    fi
  done < "$list"
  rm -f "$list"
  [ -n "$workspace" ] || { echo "provision: no workspace for $tenant under $state_root" >&2; exit 1; }

  target=$identity_dir/$tenant
  marker=$workspace/.ssh/identity-managed
  live=$workspace/.ssh/id_rsa
  if [ -f "$live" ] && [ "$(digest "$live")" != "$new_sha" ] && [ "$force" -eq 0 ]; then
    echo "provision: $tenant already holds a key of its own ($live); pass --force to replace it" >&2
    exit 1
  fi
  if [ "$apply" -eq 0 ]; then
    echo "would   $tenant: install $key_file as $target (0600), remove $marker"
    if [ -f "$live" ]; then
      echo "would   $tenant: replace $live with the same bytes (it is not the provisioned key yet)"
    else
      echo "would   $tenant: seed $live with the same bytes (the account's own copy, 0600)"
    fi
    if [ "$restart" -eq 1 ]; then
      echo "would   $tenant: bin/dshgw --config $config tenant restart $tenant"
    fi
  else
    mkdir -p "$identity_dir"
    chmod 0700 "$identity_dir"
    (umask 077; cat "$key_file" > "$target")
    chmod 0600 "$target"
    echo "seeded  $tenant: $target"
    rm -f "$marker"
    # The account's own copy is what its dsh plugin and the gateway's sshfs both read. Write it
    # now instead of waiting for a worker restart: a restart interrupts whatever the person is
    # doing, and this is byte-for-byte the write EnsureIdentity would perform on their behalf
    # (it finds a key in place afterwards and changes nothing).
    mkdir -p "$workspace/.ssh"
    chmod 0700 "$workspace/.ssh"
    (umask 077; cat "$target" > "$live")
    chmod 0600 "$live"
    echo "seeded  $tenant: $live"
    if [ "$restart" -eq 1 ]; then
      bin/dshgw --config "$config" tenant restart "$tenant"
      echo "restarted $tenant"
    fi
    if [ "$(digest "$live")" != "$new_sha" ]; then
      echo "seeded  $tenant: WARNING $live does not hold the provisioned key" >&2
      exit 1
    fi
  fi
  echo "public key to authorise on the hosts this account may reach:"
  ssh-keygen -y -f "$key_file" 2>/dev/null | sed 's/^/  /' || true
  echo "  (ssh-copy-id -i $target.pub <host>, or append it to the remote ~/.ssh/authorized_keys)"
  echo "  (until then this account can reach no host: the key is the boundary)"
}

# trim_seeds drops the operator's own host inventory from every alias list.
#
# The seeds were copies of the operator's ~/.ssh/config — which handed each account every host
# address, username and port the operator knows, before any key was even considered. What
# survives is what the account added itself: an alias its own file declares that no seed ever
# contained. Both the seed (what a future account starts from) and the live
# <workspace>/.ssh/config (what this account's plugin and the gateway read today) are rewritten
# from that rule, each snapshotted first, so nothing an account wrote is lost silently.
trim_seeds() {
  [ -d "$seed_dir" ] || { echo "trim-seeds: $seed_dir is not a directory" >&2; exit 1; }
  python3 - "$seed_dir" "$state_root" "$apply" "${USER:-$(id -un)}" <<'PY'
import json
import os
import shutil
import sys
from pathlib import Path

seed_dir, state_root, apply = Path(sys.argv[1]), Path(sys.argv[2]), int(sys.argv[3]) == 1
stamp = os.environ.get("TRIM_STAMP") or __import__("datetime").datetime.now().strftime("%Y%m%d-%H%M%S")

def parse(text):
    """Parse the alias blocks dshgw writes: Host, then HostName/User/Port."""
    hosts, current = [], None
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, _, value = line.partition(" ")
        key, value = key.strip().lower(), value.strip()
        if key == "host":
            name = value.split()[0] if value.split() else ""
            current = None
            if name and not any(c in name for c in "*?!"):
                current = {"name": name, "hostname": "", "user": "", "port": ""}
                hosts.append(current)
            continue
        if current is None:
            continue
        if key in ("hostname", "user", "port"):
            current[key] = value
    return hosts

def render(hosts):
    out = [
        "# Generated by dshgw: host aliases only. This account's identity is its own",
        "# ~/.ssh/id_rsa; per-host keys and other local settings are not carried over.",
    ]
    for host in hosts:
        out.append("")
        out.append("Host " + host["name"])
        if host["hostname"]:
            out.append("  HostName " + host["hostname"])
        if host["user"]:
            out.append("  User " + host["user"])
        if host["port"]:
            out.append("  Port " + host["port"])
    return "\n".join(out) + "\n"

seeds = [p for p in sorted(seed_dir.iterdir()) if p.is_file() and not p.name.startswith(".")]
inventory = set()
for seed in seeds:
    for host in parse(seed.read_text(encoding="utf-8", errors="replace")):
        inventory.add(host["name"])
print(f"operator inventory ({len(inventory)} aliases): {' '.join(sorted(inventory))}")

registry = state_root / "state" / "registry.json"
workspaces = {}
if registry.is_file():
    for tenant in json.loads(registry.read_text()).get("tenants", []):
        if tenant.get("workspace"):
            workspaces[tenant["name"]] = Path(tenant["workspace"])
else:
    for path in sorted((state_root / "state" / "workspaces").iterdir()):
        workspaces[path.name] = path

# The inventory record is written next to the seeds: it is the input this run used, and a later
# account cannot tell "the operator meant to hand this host over" from "this came from the old
# copy" without it.
record = seed_dir / (".operator-inventory-" + stamp)
if apply:
    record.write_text("\n".join(sorted(inventory)) + "\n", encoding="utf-8")

kept_total = 0
for account in sorted(set(list(workspaces) + [p.name for p in seeds])):
    workspace = workspaces.get(account)
    live = workspace / ".ssh" / "config" if workspace else None
    declared = []
    if live is not None and live.is_file():
        declared = parse(live.read_text(encoding="utf-8", errors="replace"))
    elif (seed_dir / account).is_file():
        declared = parse((seed_dir / account).read_text(encoding="utf-8", errors="replace"))
    kept = [host for host in declared if host["name"] not in inventory]
    dropped = [host["name"] for host in declared if host["name"] in inventory]
    kept_total += len(kept)
    print(f"{account:22s} keeps {len(kept)} ({', '.join(h['name'] for h in kept) or '-'}), drops {len(dropped)} from the operator inventory")
    if not apply:
        continue
    if live is not None and live.is_file():
        shutil.copy2(live, seed_dir / f".pre-trim-{account}-{stamp}")
    if (seed_dir / account).is_file():
        shutil.copy2(seed_dir / account, seed_dir / f".pre-trim-seed-{account}-{stamp}")
    (seed_dir / account).write_text(render(kept), encoding="utf-8")
    os.chmod(seed_dir / account, 0o644)
    if live is not None and live.is_file():
        live.write_text(render(kept), encoding="utf-8")
        os.chmod(live, 0o644)
print(f"{'wrote' if apply else 'would write'} seeds and live alias lists ({kept_total} aliases kept); snapshots: {seed_dir}/.pre-trim-*-{stamp}")
PY
}

case "$command" in
  purge-shared) purge_shared ;;
  provision) provision ;;
  trim-seeds) trim_seeds ;;
  *) usage >&2; exit 2 ;;
esac
