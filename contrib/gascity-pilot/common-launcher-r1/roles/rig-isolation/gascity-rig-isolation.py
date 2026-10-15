#!/usr/bin/env python3
"""Fail closed on rig-root launches; provision worktrees by rig, bead and session.

This tool never resets, cleans, switches, stashes, or commits a rig root.
Call ensure after resolving the work bead, then pass its path to the worker.
Install guard as a Gas City worker pre_start command. Preserve current workers
by suspending replacement launches before changing their template configuration.
"""

import argparse
import fcntl
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path


class IsolationError(RuntimeError):
    """The requested workspace cannot prove isolation or ownership."""


def git(repo, *args):
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    executable = shutil.which("git")
    if executable is None:
        raise IsolationError("Git is unavailable")
    # Argument vector only; no shell interprets bead, session, or path values.
    result = subprocess.run(  # noqa: S603
        [executable, "-C", str(repo), *args],
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode:
        raise IsolationError(result.stderr.strip() or "Git command failed")
    return result.stdout.strip()


def common(repo):
    return Path(git(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")).resolve()


def guard(rig_root, work_dir):
    if not str(rig_root).strip() or not str(work_dir).strip():
        raise IsolationError("rig root and worker path must be explicit")
    root = Path(rig_root).resolve(strict=True)
    work = Path(work_dir).resolve(strict=True)
    top = Path(git(work, "rev-parse", "--show-toplevel")).resolve()
    if work == root or top == root:
        raise IsolationError(f"worker launch rejected: {work} targets rig root {root}")
    if top != work or not (work / ".git").is_file():
        raise IsolationError("worker launch requires the root of a linked Git worktree")
    if common(root) != common(work):
        raise IsolationError("worker worktree belongs to another repository")
    branch = git(work, "symbolic-ref", "--quiet", "--short", "HEAD")
    if branch in ("main", "master"):
        raise IsolationError("worker launch requires a task branch, not main")
    return work


def identifier(value):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,119}", value) or ".." in value:
        raise IsolationError(f"invalid rig/bead/session identifier: {value!r}")
    return value


def ensure(rig_root, worktrees_root, rig, bead, session, base):
    root = Path(rig_root).resolve(strict=True)
    rig, bead, session = map(identifier, (rig, bead, session))
    pool = Path(worktrees_root).resolve()
    if pool == root or root in pool.parents:
        raise IsolationError("worktree pool must be outside the rig root")
    path = pool / rig / bead / session
    branch = f"codex/{bead}/{rig}/{session}"
    # Serialize both creation and the ownership receipt. No shared stash is used.
    with (common(root) / "gascity-isolation.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        expected = {
            "schema": 1,
            "rig": rig,
            "bead": bead,
            "session": session,
            "repo": str(common(root)),
            "path": str(path),
            "branch": branch,
        }
        if path.exists() or path.is_symlink():
            guard(root, path)
            private = Path(git(path, "rev-parse", "--absolute-git-dir"))
            receipt = private / "gascity-isolation.json"
            if not receipt.is_file() or json.loads(receipt.read_text()) != expected:
                raise IsolationError("existing worktree has different or missing ownership")
            if git(path, "symbolic-ref", "--short", "HEAD") != branch:
                raise IsolationError("owned worktree branch changed; refusing reuse")
            return path
        resolved_base = git(root, "rev-parse", "--verify", f"{base}^{{commit}}")
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.parent.resolve() != path.parent:
            raise IsolationError("worktree destination contains a symlink")
        # Git refuses branch collisions; existing branches are never overwritten.
        git(root, "worktree", "add", "-b", branch, str(path), resolved_base)
        guard(root, path)
        private = Path(git(path, "rev-parse", "--absolute-git-dir"))
        receipt = private / "gascity-isolation.json"
        with receipt.open("x") as handle:
            json.dump(expected, handle, indent=2)
            handle.write("\n")
        return path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    check = sub.add_parser("guard", help="reject roots, foreign repositories and main")
    check.add_argument("--rig-root", required=True)
    check.add_argument("--work-dir", required=True)
    create = sub.add_parser("ensure", help="create or verify a rig/bead/session worktree")
    create.add_argument("--rig-root", required=True)
    create.add_argument("--worktrees-root", required=True)
    create.add_argument("--rig", required=True)
    create.add_argument("--bead", required=True)
    create.add_argument("--session", required=True)
    create.add_argument("--base", required=True, help="explicit locally verified commit or ref")
    args = vars(parser.parse_args())
    action = args.pop("command")
    try:
        path = guard(**args) if action == "guard" else ensure(**args)
        print(json.dumps({"work_dir": str(path), "isolated": True}))
    except (IsolationError, OSError, ValueError) as error:
        print(f"rig-isolation: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
