#!/usr/bin/env python3
"""Candidate protected provider boundary for already-bound native workspaces.

Configure an external launch directory and use `check` in pre_start. Wrap the
provider command with `launch -- COMMAND ...`. No provider runs on failure.
"""

import argparse
import importlib.util
import json
import os
import stat
import subprocess
import sys
import uuid
from pathlib import Path

SPEC = importlib.util.spec_from_file_location("isolation", Path(__file__).with_name("gascity-rig-isolation.py"))
iso = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(iso)


def declared_launch_context(env, city=None):
    """Restore only an explicitly bound session's missing resume environment.

    Explicit session attach currently reseeds city/session identity but omits
    rig context. A Mayor-authored declaration supplies those inputs; it does
    not bypass the fresh authority, exact claim, or isolated-worktree guards.
    """
    city = (city or Path(__file__).resolve().parents[2]).resolve(strict=True)
    session = iso.identifier(env["GC_SESSION_ID"])
    declaration = city / ".gc/runtime/worker-launch-context" / f"{session}.json"
    try:
        file_stat = declaration.lstat()
    except FileNotFoundError:
        return env
    if not stat.S_ISREG(file_stat.st_mode):
        raise iso.IsolationError("worker launch declaration must be a regular file, not a symlink")
    if declaration.parent.resolve(strict=True) != declaration.parent:
        raise iso.IsolationError("worker launch declaration directory must not contain symlinks")
    directory_stat = declaration.parent.stat()
    if any(info.st_uid != os.getuid() or info.st_mode & 0o022
           for info in (file_stat, directory_stat)):
        raise iso.IsolationError("worker launch declaration has unsafe ownership or permissions")
    record = json.loads(declaration.read_text())
    if not isinstance(record, dict) or record.get("schema") != 1 or record.get("session") != session:
        raise iso.IsolationError("worker launch declaration session mismatch")
    for key, expected in (
        ("GC_CITY_PATH", str(city)),
        ("GC_AGENT", record.get("agent")),
        ("GC_TEMPLATE", record.get("template")),
    ):
        if not expected or env.get(key) != expected:
            raise iso.IsolationError(f"worker launch declaration {key} mismatch")
    supplied = record.get("env")
    required = {
        "GC_RIG", "GC_RIG_ROOT", "GC_BIN", "GC_ISOLATION_WORKTREES",
        "GC_TRIGGER_WORK_BEAD_ID", "GC_TRIGGER_WORK_BEAD_STORE_REF",
    }
    if not isinstance(supplied, dict) or set(supplied) != required:
        raise iso.IsolationError("worker launch declaration has incomplete or unexpected inputs")
    if any(not isinstance(value, str) or not value.strip() for value in supplied.values()):
        raise iso.IsolationError("worker launch declaration has empty inputs")
    rig = iso.identifier(supplied["GC_RIG"])
    iso.identifier(supplied["GC_TRIGGER_WORK_BEAD_ID"])
    if supplied["GC_TRIGGER_WORK_BEAD_STORE_REF"] != f"rig:{rig}":
        raise iso.IsolationError("worker launch declaration rig store mismatch")
    # The first launch must use the declared external entry directory. On a
    # later resume, accept only the exact task/session path; prepare() verifies
    # its Git ownership receipt before the provider can run there.
    pool = Path(supplied["GC_ISOLATION_WORKTREES"]).resolve()
    owned = pool / rig / supplied["GC_TRIGGER_WORK_BEAD_ID"] / session
    directory = Path(env["GC_DIR"]).resolve(strict=True)
    launch_directory = Path(record["launch_dir"]).resolve(strict=True)
    if directory not in (launch_directory, owned):
        raise iso.IsolationError("worker launch declaration directory mismatch")
    restored = dict(env)
    for key, value in supplied.items():
        if env.get(key) not in (None, "", value):
            raise iso.IsolationError(f"worker launch declaration conflicts with {key}")
        restored[key] = value
    canonical_store = supplied["GC_TRIGGER_WORK_BEAD_STORE_REF"]
    if env.get("GC_TRIGGER_WORK_STORE_REF") not in (None, "", canonical_store):
        raise iso.IsolationError("worker launch declaration conflicts with canonical trigger store")
    restored["GC_TRIGGER_WORK_STORE_REF"] = canonical_store
    if "native_thread" in record:
        native_thread = record["native_thread"]
        if not isinstance(native_thread, str) or not native_thread:
            raise iso.IsolationError("declared native conversation requires a nonempty UUID")
        try:
            if str(uuid.UUID(native_thread)) != native_thread:
                raise ValueError("noncanonical UUID")
        except ValueError as error:
            raise iso.IsolationError("declared native conversation requires its canonical UUID") from error
        restored["GC_DECLARED_NATIVE_THREAD"] = native_thread
    return restored


def verify_declared_resume(env, command):
    """Never replace a declared existing Codex conversation on resume failure."""
    native_thread = env.get("GC_DECLARED_NATIVE_THREAD")
    if not native_thread:
        return
    if Path(command[0]).name != "codex":
        raise iso.IsolationError("declared native conversation requires its original provider")
    # Parse the configured worker wrapper's global flags, consuming each value
    # before looking for a subcommand. A model/config value named 'resume' is
    # not a resume subcommand. Unknown launch shapes fail closed.
    value_flags = {"-a", "--ask-for-approval", "-c", "--config", "--model"}
    boolean_flags = {"--no-daemon", "--no-alt-screen", "--approve-for-me"}
    index = 1
    while index < len(command):
        argument = command[index]
        if argument in value_flags:
            if index + 1 >= len(command):
                raise iso.IsolationError("declared native conversation has an incomplete global option")
            index += 2
        elif argument in boolean_flags or any(argument.startswith(flag + "=") for flag in value_flags):
            index += 1
        else:
            break
    if command[index:] != ["resume", native_thread]:
        raise iso.IsolationError("declared native conversation requires its exact resume key")


def context(env):
    root = Path(env["GC_RIG_ROOT"]).resolve(strict=True)
    directory = Path(env["GC_DIR"]).resolve(strict=True)
    if directory == root or root in directory.parents:
        raise iso.IsolationError("provider launch into a rig root is forbidden")
    rig = iso.identifier(env["GC_RIG"])
    # The trigger bead is optional: a demand-spawned worker has it, but a
    # standby worker (min_active / keepalive) does not, and instead adopts the
    # routed in-flight work in prepare(). Validate only when present.
    raw_bead = (env.get("GC_TRIGGER_WORK_BEAD_ID") or "").strip()
    bead = iso.identifier(raw_bead) if raw_bead else ""
    session = iso.identifier(env["GC_SESSION_ID"])
    pool = Path(env["GC_ISOLATION_WORKTREES"]).resolve()
    if pool == root or root in pool.parents:
        raise iso.IsolationError("worktree pool must be outside the rig root")
    return root, rig, bead, session, pool


def run_gc(env, *args):
    result = subprocess.run(  # noqa: S603
        [env["GC_BIN"], *args],
        env=env,
        text=True,
        capture_output=True,
        timeout=45,
        check=False,
    )
    if result.returncode:
        # CLI errors may include credentials; expose the failed operation only.
        raise iso.IsolationError(f"gc {args[0]} failed (exit {result.returncode})")
    return result.stdout


def verify_fresh_base(root, env):
    """Fail closed before claiming work when local main is behind its authority."""
    base_ref = env.get("GC_ISOLATION_BASE_REF", "refs/heads/main")
    remote = env.get("GC_ISOLATION_REMOTE", "forgejo")
    remote_ref = env.get("GC_ISOLATION_REMOTE_REF", "refs/heads/main")
    local_oid = iso.git(root, "rev-parse", base_ref)
    remote_row = iso.git(root, "ls-remote", "--exit-code", remote, remote_ref)
    remote_oid = remote_row.split()[0] if remote_row.split() else ""
    if not remote_oid:
        raise iso.IsolationError(f"authority {remote}/{remote_ref} returned no commit")
    if local_oid != remote_oid:
        raise iso.IsolationError(
            "local main is stale; fetch the rig root deliberately before launching "
            f"(local {local_oid[:12]}, {remote} {remote_oid[:12]})"
        )
    return local_oid


def read_bead(env, bead):
    """Use the supported by-ID reader; never turn read evidence into custody."""
    result = json.loads(run_gc(env, "beads", "show", bead, "--format=json"))
    row = result.get("bead", result) if isinstance(result, dict) else None
    if not isinstance(row, dict) or row.get("id") != bead:
        raise iso.IsolationError("bead readback identity mismatch")
    if row.get("status") not in ("open", "in_progress"):
        raise iso.IsolationError("terminal or unavailable owner/work item requires supported recovery")
    if not isinstance(row.get("metadata"), dict):
        raise iso.IsolationError("missing authoritative workspace binding metadata")
    return row


def bound_session(env, bead, store, path):
    """Require the controller's existing binding; a process cwd cannot set it."""
    row = read_bead(env, env["GC_SESSION_ID"])
    meta = row["metadata"]
    if row.get("issue_type") != "session" or any(meta.get(key) != expected for key, expected in (
        ("template", env["GC_TEMPLATE"]),
        ("gc.trigger_bead_id", bead), ("gc.trigger_bead_store_ref", store),
    )):
        raise iso.IsolationError("session identity/trigger binding differs; supported recovery required")
    paths = [meta[key] for key in ("work_dir", "gc.work_dir") if meta.get(key)]
    if not paths or any(value != str(path) for value in paths):
        raise iso.IsolationError("session workspace is not bound to the verified native path")
    native = env.get("GC_DECLARED_NATIVE_THREAD")
    if native and meta.get("session_key") != native:
        raise iso.IsolationError("existing native identity is not bound to this session; recovery required")


def workspace_source(env, work, store):
    """Resolve source correspondence; the protected hook verifies signed authority.

    A generated execution step does not own its source's creation provenance.
    These public rows are preflight evidence only and cannot authorize a claim.
    """
    key = "gc.lifecycle.materialization.v1"
    encoded = work["metadata"].get(key)
    if encoded is None:
        return work
    lineage = json.loads(encoded)
    if not isinstance(lineage, dict) or type(lineage.get("version")) is not int or lineage["version"] != 1:
        raise iso.IsolationError("unsupported source linkage")
    source_id = iso.identifier(lineage.get("source_id", ""))
    if (lineage.get("source_store_ref") != store or lineage.get("workflow_store_ref") != store
            or lineage.get("route") != env["GC_TEMPLATE"]
            or work["metadata"].get("gc.root_store_ref") != store
            or lineage.get("state") not in ("attached", "lineage_pending")):
        raise iso.IsolationError("source linkage crosses the exact admitted route/store")
    source = work if source_id == work["id"] else read_bead(env, source_id)
    attached = json.loads(source["metadata"].get(key, "null"))
    if not isinstance(attached, dict) or attached.get("state") != "attached":
        raise iso.IsolationError("source lacks attached workflow linkage")
    fields = ("scope", "contract", "route", "workflow", "merge_strategy", "token",
              "source_id", "source_store_ref", "workflow_store_ref", "admission_receipt")
    if (type(attached.get("version")) is not int or attached["version"] != 1
            or any(not isinstance(lineage.get(field), str) or not lineage[field].strip()
                   or lineage[field] != attached.get(field) for field in fields)):
        raise iso.IsolationError("source and execution admission linkage differ")
    workflow = iso.identifier(attached.get("workflow_id", ""))
    meta = source["metadata"]
    if (meta.get("gc.routed_to") != lineage["route"] or meta.get("molecule_id") != workflow
            or meta.get("merge_strategy") != lineage["merge_strategy"]
            or meta.get("gc.workflow_id") or meta.get("workflow_id")
            or (lineage["state"] == "attached" and lineage.get("workflow_id") != workflow)
            or (lineage["state"] == "lineage_pending" and lineage.get("workflow_id"))):
        raise iso.IsolationError("source workflow attachment differs")
    return source


def native_workspace_args(env, root, pool, rig, bead, store, meta):
    """Reconstruct the existing native owner spec, without inventing provenance."""
    fields = {
        "repo": "gc.worktree_repo", "root": "gc.worktree_root", "path": "gc.work_dir",
        "branch": "gc.work_branch", "base": "gc.worktree_base_ref",
        "base-sha": "gc.worktree_base_sha", "creator": "gc.worktree_creator",
        "owner": "gc.worktree_owner", "generation": "gc.worktree_generation",
        "lifecycle": "gc.worktree_lifecycle",
    }
    if any(not isinstance(meta.get(key), str) or not meta[key].strip() for key in fields.values()):
        raise iso.IsolationError("incomplete native workspace binding; protected publication required")
    if meta.get("gc.root_store_ref", store) != store or meta["gc.worktree_lifecycle"] != "active":
        raise iso.IsolationError("workspace store or lifecycle differs")
    path = Path(meta["gc.work_dir"])
    if (not path.is_absolute() or path.resolve(strict=True) != path
            or meta["gc.worktree_repo"] != str(root)
            or meta["gc.worktree_root"] != str(pool / rig)
            or path.parent != pool / rig):
        raise iso.IsolationError("workspace binding is outside its native rig owner root")
    if meta.get("work_dir", str(path)) != str(path) or env["GC_DIR"] != str(path):
        raise iso.IsolationError("launch workspace differs from the existing native binding")
    args = [item for flag, key in fields.items() for item in ("--" + flag, meta[key])]
    return path, [*args, "--bead", bead, "--store-ref", store, "--json"]


def prepare(env):
    root, rig, bead, _session, pool = context(env)
    if not bead:
        raise iso.IsolationError("protected launch requires an exact admitted trigger; standby adoption refused")
    store = env.get("GC_TRIGGER_WORK_STORE_REF")
    if store != f"rig:{rig}" or env.get("GC_TRIGGER_WORK_BEAD_STORE_REF", store) != store:
        raise iso.IsolationError("exact canonical trigger store is missing or conflicts")
    verify_fresh_base(root, env)
    work = read_bead(env, bead)
    source = workspace_source(env, work, store)
    path, args = native_workspace_args(env, root, pool, rig, source["id"], store, source["metadata"])
    bound_session(env, bead, store, path)
    iso.guard(root, path)
    # Verify first: legacy custom receipts must never become creation evidence.
    # Ensure only reuses an already verified native owner, before protected claim.
    attempt = None
    for operation in ("verify", "ensure", "verify"):
        report = json.loads(run_gc(env, "worktree", operation, *args))
        current = report.get("provenance", {}).get("attempt_id")
        if (report.get("ok") is not True or report.get("path") != str(path)
                or report.get("created") is not False or report.get("branch_created") is not False
                or not current or (attempt is not None and current != attempt)):
            raise iso.IsolationError("native workspace verification changed or attempted new creation")
        attempt = current
    bound_session(env, bead, store, path)
    claim = json.loads(run_gc(env, "hook", "--claim", "--json"))
    if claim.get("action") != "work" or claim.get("bead_id") != bead:
        raise iso.IsolationError("protected claim differs from exact trigger; provider not started")
    return path


def provider_command(command):
    """Let isolated Codex workers request reviewed writes to protected Git metadata."""
    if Path(command[0]).name != "codex":
        return command

    filtered = []
    index = 1
    while index < len(command):
        argument = command[index]
        if argument in ("-a", "--ask-for-approval"):
            if index + 1 < len(command) and command[index + 1] == "never":
                index += 2
                continue
        if argument == "--ask-for-approval=never":
            index += 1
            continue
        filtered.append(argument)
        index += 1

    return [command[0], "--approve-for-me", *filtered]

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("check", "launch"))
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    try:
        env = declared_launch_context(env)
        context(env)
        if args.action == "check":
            return 0
        command = args.command
        if command[:1] == ["--"]:
            command = command[1:]
        if not command:
            raise iso.IsolationError("provider command required")
        verify_declared_resume(env, command)
        root, _, _, _, _ = context(env)
        path = prepare(env)
        command = provider_command(command)
        # Do not let an inherited git redirect silently target the rig root.
        env = {k: v for k, v in env.items() if not k.startswith("GIT_")}
        env["GC_DIR"] = env["PWD"] = str(path)
        os.chdir(path)
        os.execvpe(command[0], command, env)  # noqa: S606
    except (iso.IsolationError, OSError, KeyError, ValueError, subprocess.TimeoutExpired) as error:
        print(f"worker-isolation: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
