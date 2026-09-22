#!/usr/bin/env python3
"""Behaviour tests for scripts/dshgw_ssh_identity.sh.

The script is the repair step for the incident where one operator key was copied into every
tenant workspace, so its promises are pinned here: it only ever deletes a key that is
byte-identical to the revoked one, it stops the running gateway from putting that key back
(the identity-managed marker), it refuses to run while a mount still holds the old session, and
provision refuses to hand out the revoked key again.

    python3 scripts/test_dshgw_ssh_identity.py
"""

from __future__ import annotations

import json
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SCRIPT = REPO / "scripts" / "dshgw_ssh_identity.sh"

failures: list[str] = []
checks = 0


def check(condition: bool, message: str) -> None:
    global checks
    checks += 1
    if not condition:
        failures.append(message)


def run(*args: str, cwd: Path) -> subprocess.CompletedProcess:
    return subprocess.run(["sh", str(SCRIPT), *args], cwd=cwd, capture_output=True, text=True)


def digest(path: Path) -> str:
    import hashlib

    return hashlib.sha256(path.read_bytes()).hexdigest()


class Deployment:
    """A throwaway state root shaped like the real one: a registry, one workspace per account."""

    def __init__(self, root: Path) -> None:
        self.root = root
        self.state = root / "state"
        self.workspaces = self.state / "workspaces"
        self.workspaces.mkdir(parents=True)
        self.revoked = root / "operator-id_rsa"
        self.revoked.write_bytes(b"OPERATOR PRIVATE KEY\n")
        self.revoked.chmod(0o600)
        self.tenants: list[str] = []

    def account(self, name: str, key: bytes | None) -> Path:
        home = self.workspaces / name / ".ssh"
        home.mkdir(parents=True, exist_ok=True)
        (home / "config").write_text("Host aipc\n", encoding="utf-8")
        if key is not None:
            (home / "id_rsa").write_bytes(key)
            (home / "id_rsa").chmod(0o600)
        self.tenants.append(name)
        return home

    def write_registry(self) -> None:
        document = {
            "version": 1,
            "tenants": [
                {"name": name, "workspace": str(self.workspaces / name)} for name in self.tenants
            ],
        }
        (self.state / "registry.json").write_text(json.dumps(document), encoding="utf-8")

    def write_mounts(self, mounts: list[dict]) -> None:
        (self.state / "ssh-mounts.json").write_text(
            json.dumps({"version": 1, "mounts": mounts}), encoding="utf-8"
        )


def make_deployment(root: Path) -> Deployment:
    deployment = Deployment(root)
    deployment.account("dsh-colin", deployment.revoked.read_bytes())
    deployment.account("dsh-ran", deployment.revoked.read_bytes())
    deployment.account("dsh-mine", b"MY OWN KEY\n")
    deployment.account("dsh-empty", None)
    deployment.write_registry()
    deployment.write_mounts([])
    return deployment


def purge(deployment: Deployment, *extra: str) -> subprocess.CompletedProcess:
    return run(
        "purge-shared",
        "--state-root", str(deployment.root),
        "--revoked-key", str(deployment.revoked),
        *extra,
        cwd=deployment.root,
    )


def main() -> int:
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        deployment = make_deployment(root)
        key_hash = digest(deployment.revoked)

        # A dry run reports exactly the accounts holding the revoked key, and writes nothing.
        dry = purge(deployment)
        check(dry.returncode == 0, f"dry run failed: {dry.stderr}")
        check(f"revoked identity sha256 {key_hash}" in dry.stdout, "the dry run states the digest it matches")
        check("would   dsh-colin" in dry.stdout and "would   dsh-ran" in dry.stdout,
              f"both accounts holding the key are listed: {dry.stdout!r}")
        check("would   dsh-mine" not in dry.stdout and "would   dsh-empty" not in dry.stdout,
              "an account with its own key, or none, is not a purge candidate")
        check("keep    dsh-mine" in dry.stdout, "an account's own key is reported as kept, not deleted")
        check("plan: 2 of 3 keys are the revoked identity" in dry.stdout, f"the summary counts keys: {dry.stdout!r}")
        for name in ("dsh-colin", "dsh-ran", "dsh-mine"):
            check((deployment.workspaces / name / ".ssh" / "id_rsa").exists(),
                  f"a dry run left {name}'s key alone")

        # A recorded mount means the old session may still be live: refuse unless told otherwise.
        deployment.write_mounts([{"tenant": "dsh-colin", "mountpoint": "/w/ssh/aipc/app"}])
        blocked = purge(deployment, "--apply")
        check(blocked.returncode == 1, "a recorded mount stops the purge")
        check("--allow-mounted" in blocked.stderr, f"the refusal names the override: {blocked.stderr!r}")
        check("dsh-colin" not in blocked.stdout, "nothing is deleted when the purge refuses")
        deployment.write_mounts([])

        # The real thing: the revoked copies go, the marker appears, everything else survives.
        applied = purge(deployment, "--apply")
        check(applied.returncode == 0, f"purge failed: {applied.stderr}")
        check("done: 2 of 3 keys reclaimed" in applied.stdout, f"summary: {applied.stdout!r}")
        for name in ("dsh-colin", "dsh-ran"):
            home = deployment.workspaces / name / ".ssh"
            check(not (home / "id_rsa").exists(), f"{name}'s revoked key was deleted")
            marker = home / "identity-managed"
            check(marker.exists(), f"{name} got an identity-managed marker")
            check(stat.S_IMODE(marker.stat().st_mode) == 0o600, f"{name}'s marker is 0600")
            check((home / "config").exists(), f"{name}'s alias list is untouched")
        mine = deployment.workspaces / "dsh-mine" / ".ssh"
        check((mine / "id_rsa").read_bytes() == b"MY OWN KEY\n", "an account's own key is never touched")
        check(not (mine / "identity-managed").exists(), "an account's own key gets no marker")

        # Idempotent, and usable after the revoked key file itself has been replaced.
        again = purge(deployment, "--apply")
        check("done: 0 of 1 keys reclaimed" in again.stdout, f"a second purge deletes nothing: {again.stdout!r}")
        by_hash = run(
            "purge-shared", "--state-root", str(deployment.root),
            "--revoked-sha256", key_hash, cwd=deployment.root,
        )
        check(by_hash.returncode == 0, f"matching by digest failed: {by_hash.stderr}")
        check("plan: 0 of 1" in by_hash.stdout, "the digest form matches the same (now absent) keys")
        # A state root with no registry falls back to the directory listing.
        (deployment.state / "registry.json").unlink()
        fallback = purge(deployment)
        check(fallback.returncode == 0 and "revoked identity" in fallback.stdout,
              f"the directory fallback still runs: {fallback.stderr!r}")

    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        deployment = make_deployment(root)
        # The repair comes first: provisioning an account that still holds the revoked copy is
        # exactly what must not happen silently.
        purge(deployment, "--apply")
        identity_dir = root / "ssh-keys"
        corrupt = root / "revoked-copy"
        corrupt.write_bytes(deployment.revoked.read_bytes())

        # Provision must refuse the very key this repair exists to remove.
        refused = run(
            "provision", "--tenant", "dsh-colin", "--key", str(corrupt),
            "--state-root", str(deployment.root), "--revoked-key", str(deployment.revoked),
            "--identity-dir", str(identity_dir), cwd=deployment.root,
        )
        check(refused.returncode == 1, "provisioning the revoked key fails")
        check("IS the revoked key" in refused.stderr, f"the refusal says why: {refused.stderr!r}")
        check(not identity_dir.exists(), "a refused provision writes nothing")

        # One key per account: a key already issued to another account is refused too.
        have_ssh_keygen = shutil.which("ssh-keygen") is not None
        if have_ssh_keygen:
            for name in ("dsh-colin", "dsh-ran"):
                generated = root / f"{name}.key"
                made = subprocess.run(
                    ["ssh-keygen", "-t", "ed25519", "-N", "", "-C", f"{name}@test", "-f", str(generated)],
                    capture_output=True, text=True,
                )
                check(made.returncode == 0, f"generating {name}'s key: {made.stderr}")
                provisioned = run(
                    "provision", "--tenant", name, "--key", str(generated),
                    "--state-root", str(deployment.root), "--revoked-key", str(deployment.revoked),
                    "--identity-dir", str(identity_dir), "--apply", cwd=deployment.root,
                )
                check(provisioned.returncode == 0, f"provision {name} failed: {provisioned.stderr}")
                target = identity_dir / name
                check(target.exists(), f"{name}'s key was installed as identity_dir/<account>")
                check(stat.S_IMODE(target.stat().st_mode) == 0o600, f"{name}'s provisioned key is 0600")
                check(target.read_bytes() == generated.read_bytes(), f"{name}'s key bytes are the supplied ones")
                check(stat.S_IMODE(identity_dir.stat().st_mode) == 0o700, "the key directory is 0700")
                check(not (deployment.workspaces / name / ".ssh" / "identity-managed").exists(),
                      f"{name}'s marker was dropped so the gateway seeds the key")
                live = deployment.workspaces / name / ".ssh" / "id_rsa"
                check(live.exists() and live.read_bytes() == generated.read_bytes(),
                      f"{name} got its own copy of the provisioned key without a worker restart")
                check(stat.S_IMODE(live.stat().st_mode) == 0o600, f"{name}'s own key copy is 0600")
                check("AAAAB3NzaC1lZDI1NTE5" in provisioned.stdout or "ssh-ed25519" in provisioned.stdout,
                      f"the public half is printed for the operator: {provisioned.stdout!r}")
                check(f"ssh-copy-id -i {target}.pub" in provisioned.stdout,
                      "the remote-authorisation step is named")
            shared = run(
                "provision", "--tenant", "dsh-mine", "--key", str(root / "dsh-ran.key"),
                "--state-root", str(deployment.root), "--revoked-key", str(deployment.revoked),
                "--identity-dir", str(identity_dir), cwd=deployment.root,
            )
            check(shared.returncode == 1, "the same key is refused for a second account")
            check("one key per account" in shared.stderr, f"the refusal says why: {shared.stderr!r}")

            # A dry run reports the plan and writes nothing.
            dry = run(
                "provision", "--tenant", "dsh-empty", "--key", str(root / "dsh-colin.key"),
                "--state-root", str(deployment.root), "--revoked-key", str(deployment.revoked),
                "--identity-dir", str(root / "unused-keys"), cwd=deployment.root,
            )
            check(dry.returncode == 0 and "would   dsh-empty" in dry.stdout, f"dry-run plan: {dry.stdout!r}")
            check(not (root / "unused-keys").exists(), "the dry run wrote nothing")

        # An account that already holds a key of its own is not silently overwritten.
        existing = deployment.workspaces / "dsh-mine" / ".ssh" / "id_rsa"
        spare = root / "dsh-mine.key"
        subprocess.run(
            ["ssh-keygen", "-t", "ed25519", "-N", "", "-C", "dsh-mine@test", "-f", str(spare)],
            capture_output=True, text=True,
        )
        keep = run(
            "provision", "--tenant", "dsh-mine", "--key", str(spare),
            "--state-root", str(deployment.root), "--revoked-key", str(deployment.revoked),
            "--identity-dir", str(identity_dir), "--apply", cwd=deployment.root,
        ) if have_ssh_keygen else None
        if keep is not None:
            check(keep.returncode == 1, "an account's own key is not replaced without --force")
            check("already holds a key of its own" in keep.stderr, f"the refusal says why: {keep.stderr!r}")
            check(existing.read_bytes() == b"MY OWN KEY\n", "the account's own key is untouched")
            forced = run(
                "provision", "--tenant", "dsh-mine", "--key", str(root / "dsh-colin.key"),
                "--state-root", str(deployment.root), "--revoked-key", str(deployment.revoked),
                "--identity-dir", str(identity_dir), "--apply", "--force", cwd=deployment.root,
            )
            check(forced.returncode == 1, "the same key is still refused for two accounts even with --force")
            check("one key per account" in forced.stderr, f"the refusal says why: {forced.stderr!r}")

        # An unknown account is an error rather than a silent no-op.
        stranger_key = root / "stranger.key"
        stranger_key.write_bytes(b"SOME OTHER KEY\n")
        unknown = run(
            "provision", "--tenant", "dsh-nobody", "--key", str(stranger_key),
            "--state-root", str(deployment.root),
            "--revoked-key", str(deployment.revoked),
            "--identity-dir", str(identity_dir), cwd=deployment.root,
        )
        check(unknown.returncode == 1, "an unknown account fails")
        check("no workspace for dsh-nobody" in unknown.stderr, f"the reason is stated: {unknown.stderr!r}")

    # A per-host copy is the same exposure as the account default, and it is easy to miss: it
    # is not the path the configuration key named, and the copy in this deployment differed from
    # the original by one trailing newline — a different digest, the same keypair. The purge has
    # to match by fingerprint, not by bytes.
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        if shutil.which("ssh-keygen") is not None:
            deployment = Deployment(root)
            retired = root / "retired-operator-key"
            made = subprocess.run(
                ["ssh-keygen", "-t", "rsa", "-b", "2048", "-N", "", "-C", "operator@test", "-f", str(retired)],
                capture_output=True, text=True,
            )
            check(made.returncode == 0, f"generating the operator key: {made.stderr}")
            home = deployment.account("dsh-host-key", b"ANOTHER KEY\n")
            host_dir = home / "host_keys" / "deadbeef"
            host_dir.mkdir(parents=True)
            (host_dir / "id_rsa").write_bytes(retired.read_bytes() + b"\n")
            (host_dir / "id_rsa").chmod(0o600)
            deployment.write_registry()
            deployment.write_mounts([])
            check(digest(host_dir / "id_rsa") != digest(retired),
                  "the per-host copy differs from the revoked key byte for byte")

            def purge_retired(*extra: str) -> subprocess.CompletedProcess:
                return run(
                    "purge-shared", "--state-root", str(deployment.root),
                    "--revoked-key", str(retired), *extra, cwd=deployment.root,
                )

            plan = purge_retired()
            check("would   dsh-host-key: delete" in plan.stdout and "host_keys" in plan.stdout,
                  f"the per-host copy is a purge candidate: {plan.stdout!r}")
            check("plan: 1 of 2 keys" in plan.stdout, f"the summary counts keys, not accounts: {plan.stdout!r}")
            check("fingerprint" in plan.stdout, "the plan states the fingerprint it matches")

            purge_retired("--apply")
            check(not (host_dir / "id_rsa").exists(), "the per-host copy is gone")
            check(not (home / "identity-managed").exists(),
                  "a per-host key does not write the account-default marker")

    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        deployment = make_deployment(root)
        seeds = root / "ssh-configs"
        seeds.mkdir()
        inventory = (
            "Host aipc\n  HostName 10.0.0.1\n  User ops\n\n"
            "Host gpt001\n  HostName gpt001.example\n  User root\n  Port 2222\n"
        )
        (seeds / "dsh-colin").write_text(inventory, encoding="utf-8")
        (seeds / "dsh-ran").write_text(inventory, encoding="utf-8")
        # dsh-colin's live list is the operator inventory plus one alias it added itself;
        # dsh-ran never added anything.
        own = "\nHost my-server\n  HostName 10.9.9.9\n  User me\n"
        (deployment.workspaces / "dsh-colin" / ".ssh" / "config").write_text(inventory + own, encoding="utf-8")
        (deployment.workspaces / "dsh-ran" / ".ssh" / "config").write_text(inventory, encoding="utf-8")

        def trim(*extra: str) -> subprocess.CompletedProcess:
            return run(
                "trim-seeds", "--seed-dir", str(seeds), "--state-root", str(deployment.root),
                *extra, cwd=deployment.root,
            )

        dry = trim()
        check(dry.returncode == 0, f"the trim plan failed: {dry.stderr}")
        check("keeps 1 (my-server)" in dry.stdout, f"only the account's own alias is kept: {dry.stdout!r}")
        check("would write seeds" in dry.stdout, "the plan says what it would write")
        colin_config = deployment.workspaces / "dsh-colin" / ".ssh" / "config"
        check("my-server" in colin_config.read_text(encoding="utf-8"), "a dry run changes nothing")

        applied = trim("--apply")
        check(applied.returncode == 0, f"the trim failed: {applied.stderr}")
        colin_config = colin_config.read_text(encoding="utf-8")
        check("my-server" in colin_config, "the account's own alias survives the trim")
        check("aipc" not in colin_config and "gpt001" not in colin_config,
              f"the operator inventory is gone from the live list: {colin_config!r}")
        check("my-server" in (seeds / "dsh-colin").read_text(encoding="utf-8"),
              "the seed keeps the account's own alias too")
        check("aipc" not in (seeds / "dsh-colin").read_text(encoding="utf-8"),
              "the seed no longer carries the operator inventory")
        ran_seed = (seeds / "dsh-ran").read_text(encoding="utf-8")
        check("Host" not in ran_seed, f"an account that added nothing ends up with no aliases: {ran_seed!r}")
        check(any(p.name.startswith(".pre-trim-dsh-colin-") for p in seeds.iterdir()),
              "the live alias list was snapshotted before it was rewritten")
        check(any(p.name.startswith(".operator-inventory-") for p in seeds.iterdir()),
              "the inventory the run used is recorded next to the seeds")

    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print(f"dshgw_ssh_identity: {checks} assertions passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
