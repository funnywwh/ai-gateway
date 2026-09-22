#!/bin/sh
# Adopt each account's existing ssh config as that account's per-account seed.
#
# Before ssh_config_dir existed, one host-wide file (in this deployment, the operator's own
# ~/.ssh/config) was copied into every account's <workspace>/.ssh/config. ssh_config_dir
# replaces it with one file per account, and this script preserves what each account uses
# today as that account's seed, so switching does not take away an alias a mount or a session
# relies on. Trim each seed afterwards to the hosts that account may reach.
#
# What it does NOT do: touch any account workspace, start or restart anything, or read the
# deployment account's own ~/.ssh. The gateway reads <workspace>/.ssh/config directly, so the
# running deployment keeps working with or without this step.
#
#   scripts/ssh_config_adopt.sh [--workspaces DIR] [--dest DIR] [--dry-run]
#
# Defaults match the local deployment (docs/deployment-layout.md). A seed that already exists
# is never overwritten: the operator's own file wins over an automatic one.
set -eu

workspaces=data/dshgw-verify/state/workspaces
dest=data/dshgw-verify/ssh-configs
dry_run=0

usage() {
  cat <<'EOF'
usage: ssh_config_adopt.sh [--workspaces DIR] [--dest DIR] [--dry-run]

  --workspaces DIR  directory holding one workspace per account
                    (default: data/dshgw-verify/state/workspaces)
  --dest DIR        ssh_config_dir to seed (default: data/dshgw-verify/ssh-configs)
  --dry-run         print what would be adopted and write nothing
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --workspaces) workspaces=$2; shift 2 ;;
    --dest) dest=$2; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

if [ ! -d "$workspaces" ]; then
  echo "adopt: $workspaces is not a directory" >&2
  exit 1
fi

adopted=0
skipped=0
missing=0

for workspace in "$workspaces"/*; do
  [ -d "$workspace" ] || continue
  account=$(basename "$workspace")
  source=$workspace/.ssh/config
  if [ ! -f "$source" ]; then
    echo "skip    $account: no $source"
    missing=$((missing + 1))
    continue
  fi
  target=$dest/$account
  if [ -e "$target" ]; then
    echo "keep    $account: $target already exists"
    skipped=$((skipped + 1))
    continue
  fi
  if [ "$dry_run" -eq 1 ]; then
    echo "would   $account: $source -> $target"
    adopted=$((adopted + 1))
    continue
  fi
  mkdir -p "$dest"
  cp "$source" "$target"
  # 0644 is the documented seed mode: the file holds aliases, not secrets, and the gateway
  # refuses a world-writable one.
  chmod 0644 "$target"
  echo "adopt   $account: $source -> $target"
  adopted=$((adopted + 1))
done

if [ "$adopted" -eq 0 ] && [ "$dry_run" -eq 0 ] && [ ! -d "$dest" ]; then
  # The configured directory has to exist even when there is nothing to seed.
  mkdir -p "$dest"
  echo "create  $dest (no account had a config to adopt)"
fi

echo "ssh_config_adopt: adopted=$adopted kept=$skipped without-config=$missing dest=$dest dry-run=$dry_run"
echo "next: point ssh_workspaces.ssh_config_dir at $dest, then restart dshgw."
echo "      an account keeps the config it already has; to re-seed one account, replace its"
echo "      seed, delete <workspace>/.ssh/config and restart that account's worker."
