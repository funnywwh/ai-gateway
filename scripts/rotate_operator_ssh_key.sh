#!/bin/sh
# Rotate the deployment account's own ssh key after it was handed to tenants.
#
# ssh_workspaces.identity_source used to copy ONE operator key into every tenant workspace. In
# this deployment it was the deployment account's own ~/.ssh/id_rsa, and that key is authorised
# on the gateway host itself — so every tenant could authenticate here (and on every other host
# that trusts it) simply by reading its own workspace. Reclaiming the copies
# (scripts/dshgw_ssh_identity.sh) closes that door from the tenant side; this script closes it
# from the key side, because the key itself has to be assumed read by eight sandboxes.
#
#   scripts/rotate_operator_ssh_key.sh [--apply] [--host ALIAS]... [--seed-config DIR]
#                                     [--old-key FILE] [--new-key FILE] [--archive-dir DIR]
#   scripts/rotate_operator_ssh_key.sh --rollback [--archive FILE]
#
# Per host the order is deliberate: authorise the new key, PROVE it works, and only then remove
# the old one, so no host ever ends up with neither. A host that fails at any step is reported
# and left exactly as it was. The local host is handled through its authorized_keys file (not
# only over loopback), because it is the host the tenants escaped into and it must not depend on
# sshd accepting a connection at the moment of the rotation.
#
# Nothing is changed without --apply.
set -eu

mode=dry
rollback=0
archive=
old_key=${HOME}/.ssh/id_rsa
new_key=${HOME}/.ssh/id_rsa.new
archive_dir=${HOME}/keys
seed_dir=data/dshgw-verify/ssh-configs
hosts_extra=
connect_timeout=8
stamp=$(date +%Y%m%d-%H%M%S)

usage() {
  cat <<'EOF'
usage: rotate_operator_ssh_key.sh [--apply] [--host ALIAS]... [--seed-config DIR]
                                  [--old-key FILE] [--new-key FILE] [--archive-dir DIR]
       rotate_operator_ssh_key.sh --rollback [--archive FILE]

  Without --apply the script only probes: it prints which hosts accept the current key, which
  hosts would be rotated, and which already accept the replacement.

  --host ALIAS       an extra host to consider (repeatable); default hosts come from the alias
                     seeds in --seed-config
  --seed-config DIR  directory of per-account alias seeds (default: data/dshgw-verify/ssh-configs)
  --old-key FILE     the key being retired (default: ~/.ssh/id_rsa)
  --new-key FILE     the replacement (default: ~/.ssh/id_rsa.new; generated when missing)
  --archive-dir DIR  where the retired key is kept (default: ~/keys)
  --rollback         put the archived key back as --old-key and say what still trusts it
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --apply) mode=apply; shift ;;
    --rollback) rollback=1; shift ;;
    --host) hosts_extra="$hosts_extra $2"; shift 2 ;;
    --seed-config) seed_dir=$2; shift 2 ;;
    --old-key) old_key=$2; shift 2 ;;
    --new-key) new_key=$2; shift 2 ;;
    --archive-dir) archive_dir=$2; shift 2 ;;
    --archive) archive=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

known_hosts=${TMPDIR:-/tmp}/rotate-operator-known_hosts.$$
merged_config=${TMPDIR:-/tmp}/rotate-operator-ssh-config.$$

cleanup() { rm -f "$known_hosts" "$merged_config"; }
trap cleanup EXIT INT TERM

# ssh_base is the one shape every call uses: no user config, no agent, no other identity.
ssh_base() {
  key=$1
  printf '%s' "-F $merged_config -i $key -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$known_hosts -o IdentityAgent=none -o IdentitiesOnly=yes -o ConnectTimeout=$connect_timeout"
}

# blob prints the base64 public-key body of a private key: the part that identifies it inside an
# authorized_keys file, whatever comment that file carries.
blob_of() {
  ssh-keygen -y -f "$1" 2>/dev/null | cut -d' ' -f2
}

# pub_of prints the whole public key line of a private key.
pub_of() {
  ssh-keygen -y -f "$1" 2>/dev/null
}

# works reports whether a key authenticates on a host right now.
works() {
  key=$1
  host=$2
  [ -n "$(blob_of "$key")" ] || return 1
  # shellcheck disable=SC2046
  timeout 25 ssh $(ssh_base "$key") -- "$host" true >/dev/null 2>&1
}

# run_on runs one command on a host with the given key.
run_on() {
  key=$1
  host=$2
  command=$3
  # shellcheck disable=SC2046
  timeout 25 ssh $(ssh_base "$key") -- "$host" "$command"
}

# merge_seeds writes one ssh config holding every alias seed, so -F can resolve an alias no
# matter which account's file declared it.
merge_seeds() {
  : > "$merged_config"
  if [ -d "$seed_dir" ]; then
    for seed in "$seed_dir"/*; do
      [ -f "$seed" ] || continue
      cat "$seed" >> "$merged_config"
      printf '\n' >> "$merged_config"
    done
  fi
  chmod 600 "$merged_config"
}

# candidate_hosts prints every alias the seeds declare, plus the extra hosts, deduplicated.
candidate_hosts() {
  for host in $hosts_extra; do
    printf '%s\n' "$host"
  done
  for seed in "$seed_dir"/*; do
    [ -f "$seed" ] || continue
    awk '/^[[:space:]]*[Hh]ost[[:space:]]/ { for (i = 2; i <= NF; i++) print $i }' "$seed"
  done
}

# local_authorized_keys prints the deployment account's own authorized_keys path.
local_authorized_keys() { printf '%s\n' "${HOME}/.ssh/authorized_keys"; }

# rotate_one_host does the three-step rotation for one remote host.
rotate_one_host() {
  host=$1
  old_blob=$2
  new_pub=$3
  if ! works "$new_key" "$host"; then
    echo "authorise $host"
    printf '%s\n' "$new_pub" | run_on "$old_key" "$host" 'umask 077; mkdir -p ~/.ssh; cat >> ~/.ssh/authorized_keys'
  else
    echo "already $host"
  fi
  if ! works "$new_key" "$host"; then
    echo "FAILED  $host: the new key does not authenticate; the old key was left in place" >&2
    return 1
  fi
  echo "verify  $host: new key works"
  run_on "$new_key" "$host" "cp -p ~/.ssh/authorized_keys ~/.ssh/authorized_keys.pre-rotation-$stamp 2>/dev/null || true" >/dev/null 2>&1 || true
  echo "revoke  $host: removing the retired key from authorized_keys"
  run_on "$new_key" "$host" "tmp=\$(mktemp); grep -v -F '$old_blob' ~/.ssh/authorized_keys > \"\$tmp\" || true; cat \"\$tmp\" > ~/.ssh/authorized_keys; rm -f \"\$tmp\"; chmod 600 ~/.ssh/authorized_keys" >/dev/null
  if works "$old_key" "$host"; then
    echo "FAILED  $host: the retired key still authenticates" >&2
    return 1
  fi
  echo "done    $host: retired key refused, new key accepted"
  return 0
}

# rotate_local edits this host's own authorized_keys directly. The tenants escaped through this
# file, so it is handled even when sshd is unreachable.
rotate_local() {
  old_blob=$1
  new_pub=$2
  file=$(local_authorized_keys)
  [ -f "$file" ] || { echo "local   no $file; nothing to rotate"; return 0; }
  grep -q -F "$old_blob" "$file" || { echo "local   the retired key is not authorised here"; return 0; }
  if grep -q -F "$(printf '%s' "$new_pub" | cut -d' ' -f2)" "$file"; then
    echo "local   the new key is already authorised"
  else
    echo "authorise local: appending the new key to $file"
    if [ "$mode" = apply ]; then
      (umask 077; printf '%s\n' "$new_pub" >> "$file")
    fi
  fi
  if [ "$mode" = apply ]; then
    cp -p "$file" "$file.pre-rotation-$stamp"
    tmp=$(mktemp)
    grep -v -F "$old_blob" "$file" > "$tmp" || true
    cat "$tmp" > "$file"
    rm -f "$tmp"
    chmod 600 "$file"
    echo "revoke  local: removed the retired key from $file (backup $file.pre-rotation-$stamp)"
  else
    echo "would   local: remove the retired key from $file"
  fi
  if works "$new_key" "$(id -un)@127.0.0.1"; then
    echo "verify  local: the new key authenticates over loopback"
  else
    echo "local   the new key does not authenticate over loopback (sshd may be down); the file itself was updated"
  fi
  return 0
}

do_rollback() {
  if [ -z "$archive" ]; then
    archive=$(ls -1t "$archive_dir"/id_rsa.revoked-* 2>/dev/null | head -1)
  fi
  [ -n "$archive" ] && [ -f "$archive" ] || { echo "rollback: no archived key found (pass --archive)" >&2; exit 1; }
  echo "rollback: restoring $archive as $old_key"
  if [ "$mode" = apply ]; then
    cp -p "$archive" "$old_key"
    chmod 600 "$old_key"
    ssh-keygen -y -f "$old_key" > "$old_key.pub" 2>/dev/null || true
    chmod 644 "$old_key.pub" 2>/dev/null || true
    blob=$(blob_of "$old_key")
    file=$(local_authorized_keys)
    if [ -f "$file" ] && ! grep -q -F "$blob" "$file"; then
      printf '%s\n' "$(pub_of "$old_key")" >> "$file"
      echo "rollback: re-authorised the restored key in $file"
    fi
    if [ -f "$archive.hosts" ]; then
      echo "rollback: these hosts had the retired key removed; re-add it there if needed:"
      sed 's/^/  /' "$archive.hosts"
    fi
  fi
  return 0
}

main() {
  if [ ! -f "$old_key" ]; then
    echo "rotate: $old_key does not exist" >&2
    exit 1
  fi
  old_blob=$(blob_of "$old_key")
  [ -n "$old_blob" ] || { echo "rotate: cannot read $old_key with ssh-keygen" >&2; exit 1; }
  merge_seeds

  if [ "$rollback" -eq 1 ]; then
    do_rollback
    return 0
  fi

  if [ ! -f "$new_key" ]; then
    echo "generate $new_key"
    if [ "$mode" = apply ]; then
      mkdir -p "$(dirname "$new_key")"
      ssh-keygen -t rsa -b 4096 -N '' -C "$(id -un)@$(hostname)-$stamp" -f "$new_key" >/dev/null
      chmod 600 "$new_key"
    fi
  fi
  if [ -f "$new_key" ]; then
    new_pub=$(pub_of "$new_key")
    [ -n "$new_pub" ] || { echo "rotate: $new_key is not a usable private key" >&2; exit 1; }
  else
    new_pub=""
  fi

  # 1. Who trusts the current key? Only those hosts are rotated; the rest are reported.
  trusted=
  
  for host in $(candidate_hosts | sort -u); do
    if works "$old_key" "$host"; then
      echo "trusts  $host"
      trusted="$trusted $host"
    else
      echo "no-key  $host"

    fi
  done
  echo "local   this host's own authorized_keys is rotated separately below"

  if [ "$mode" != apply ]; then
    echo "plan: rotate the hosts above and the local authorized_keys (dry run; pass --apply)"
    return 0
  fi

  [ -n "$new_pub" ] || { echo "rotate: the replacement key is missing" >&2; exit 1; }
  failures=0
  rotated=
  for host in $trusted; do
    if rotate_one_host "$host" "$old_blob" "$new_pub"; then
      rotated="$rotated $host"
    else
      failures=$((failures + 1))
    fi
  done
  if ! rotate_local "$old_blob" "$new_pub"; then
    failures=$((failures + 1))
  fi

  # 2. Only now is the key file itself replaced: every host that trusted it has the new key.
  mkdir -p "$archive_dir"
  chmod 700 "$archive_dir"
  archive="$archive_dir/id_rsa.revoked-$stamp"
  cp -p "$old_key" "$archive"
  chmod 600 "$archive"
  printf '%s\n' "$rotated" | tr ' ' '\n' | sed '/^$/d' > "$archive.hosts"
  cat "$new_key" > "$old_key"
  chmod 600 "$old_key"
  ssh-keygen -y -f "$old_key" > "$old_key.pub" 2>/dev/null || true
  chmod 644 "$old_key.pub" 2>/dev/null || true
  rm -f "$new_key" "$new_key.pub"
  echo "swapped $old_key: the retired key is archived at $archive (never authorise it again)"

  # 3. Final matrix: the retired key is refused wherever it used to work, and the key now in
  # place works. Both halves are checked against the real hosts, not against the plan.
  echo "--- after rotation ---"
  for host in $trusted; do
    if works "$archive" "$host"; then
      echo "STILL   $host: the retired key still authenticates" >&2
      failures=$((failures + 1))
    else
      echo "retired $host: refused"
    fi
    printf 'current %s: %s\n' "$host" "$(works "$old_key" "$host" && echo accepted || echo refused)"
  done
  if [ "$failures" -gt 0 ]; then
    echo "rotate: $failures problem(s); the key file was still swapped (retired copy: $archive)" >&2
    return 1
  fi
  echo "rotate: done — $archive must never be authorised again"
  return 0
}

main
