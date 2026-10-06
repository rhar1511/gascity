#!/usr/bin/env python3
"""Run the opt-in Gas City adapter proof against disposable installed bd/Dolt.

Run from any directory after the Go environment is configured:

    python3 scripts/test-installed-private-evidence-adapter.py

The fixture starts its own loopback Dolt server and bd serve, passes only the
token-file path to the Go test, then terminates both services and the exact
proxy root created by bd init. The temporary database is removed at exit.
"""

import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

LOCAL_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def owned_proxies(workspace):
    wanted_root = str(workspace / ".beads" / "dolt")
    matches = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            args = (entry / "cmdline").read_bytes().split(b"\0")
            if len(args) > 3 and args[1] == b"db-proxy-child" and b"--root" in args:
                target = args[args.index(b"--root") + 1].decode()
                if target == wanted_root:
                    matches.append(int(entry.name))
        except (OSError, IndexError, UnicodeError):
            continue
    return matches


def run(args, cwd, env, timeout=120):
    result = subprocess.run(args, cwd=cwd, env=env, capture_output=True,
                            text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{Path(args[0]).name} command failed with exit {result.returncode}")
    return result.stdout


def remove_fixture(path):
    # Go's module cache uses readonly extracted module trees. Restore directory
    # permissions before removing this exact temporary fixture root.
    for directory, _, _ in os.walk(path):
        os.chmod(directory, 0o700)
    shutil.rmtree(path)


def request(base, token, method, path):
    req = urllib.request.Request(
        base + path,
        method=method,
        headers={"Authorization": "Bearer " + token, "Accept": "application/json"},
    )
    with LOCAL_OPENER.open(req, timeout=10) as response:
        return response.status, json.load(response)


def main():
    root = Path(tempfile.mkdtemp(prefix="gc-private-evidence-adapter-", dir="/var/tmp"))
    workspace = root / "workspace"
    workspace.mkdir()
    data = root / "data"
    data.mkdir()
    home = root / "home"
    home.mkdir()
    config = root / "config"
    config.mkdir()
    token = secrets.token_hex(32)
    token_file = root / "controller-token"
    token_file.touch(mode=0o600)
    token_file.write_text(token + "\n")
    token_file.chmod(0o600)

    blocked_names = {"all_proxy", "http_proxy", "https_proxy", "no_proxy"}
    inherited = {k: v for k, v in os.environ.items()
                 if not k.lower().startswith(("BD_", "BEADS_", "DOLT_", "GC_", "GIT_"))
                 and k.lower() not in blocked_names}
    env = dict(inherited)
    env["HOME"] = str(home)
    env["XDG_CONFIG_HOME"] = str(config)
    env["DOLT_ROOT_PATH"] = str(root / "dolt-home")
    env["GOMODCACHE"] = "/home/ricky/go/pkg/mod"
    env["GOCACHE"] = "/home/ricky/.cache/go-build"
    env["GOTMPDIR"] = "/var/tmp/gascity-astra-go"
    env["GOMAXPROCS"] = "2"
    env["TMPDIR"] = "/var/tmp"

    bd = os.environ.get("GC_PRIVATE_EVIDENCE_BD_BINARY") or shutil.which("bd")
    dolt = os.environ.get("GC_PRIVATE_EVIDENCE_DOLT_BINARY") or shutil.which("dolt")
    go = shutil.which("go")
    if not bd or not Path(bd).is_absolute() or not dolt or not go:
        remove_fixture(root)
        raise RuntimeError("an absolute installed bd binary, dolt, and go are required")

    repo = Path(__file__).resolve().parents[1]
    children = []
    report = {
        "scope": "disposable Dolt database and loopback bd serve only",
        "bd_binary": bd,
        "checks": {},
    }
    try:
        bd_version = run([bd, "version"], workspace, env).strip()
        report["bd_version"] = bd_version
        if "1.3.0" not in bd_version:
            raise RuntimeError("installed bd is not v1.3.0; re-assess its HTTP contract before running")

        db_port, api_port = port(), port()
        server = subprocess.Popen(
            [dolt, "sql-server", "--host", "127.0.0.1", "--port", str(db_port), "--data-dir", str(data)],
            cwd=workspace, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        children.append(server)
        for _ in range(100):
            if server.poll() is not None:
                raise RuntimeError("disposable Dolt server exited before readiness")
            try:
                with socket.create_connection(("127.0.0.1", db_port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError("disposable Dolt listener was not ready")

        run(["git", "init", "--quiet"], workspace, env)
        run([bd, "init", "--server", "--external", "--server-host", "127.0.0.1",
             "--server-port", str(db_port), "--server-socket", "", "--database",
             "gc_private_evidence_probe", "--prefix", "probe", "--non-interactive",
             "--skip-hooks"], workspace, env)
        env["BEADS_DIR"] = str(workspace / ".beads")

        api = f"http://127.0.0.1:{api_port}"
        service = subprocess.Popen(
            [bd, "serve", "--addr", f"127.0.0.1:{api_port}", "--auth-token-file", str(token_file)],
            cwd=workspace, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        children.append(service)
        for _ in range(100):
            if service.poll() is not None:
                raise RuntimeError("disposable bd serve exited before readiness")
            try:
                status, context = request(api, token, "GET", "/v0/beads/context")
                if status == 200:
                    break
            except (urllib.error.URLError, TimeoutError):
                time.sleep(0.1)
        else:
            raise RuntimeError("disposable bd serve listener was not ready")

        required = {"issues.casMetadata", "issues.create", "issues.get", "project.enforce"}
        missing = sorted(required - set(context.get("capabilities", [])))
        if context.get("database") != "gc_private_evidence_probe" or not context.get("project_id") or missing:
            raise RuntimeError("installed service context lacks the expected disposable scope or routes")

        test_env = dict(env)
        test_env.update({
            "GC_PRIVATE_EVIDENCE_INSTALLED_PROBE": "1",
            "GC_PRIVATE_EVIDENCE_PROBE_ENDPOINT": api,
            "GC_PRIVATE_EVIDENCE_PROBE_PROJECT_ID": context["project_id"],
            "GC_PRIVATE_EVIDENCE_PROBE_DATABASE": "gc_private_evidence_probe",
            "GC_PRIVATE_EVIDENCE_PROBE_SCOPE_REF": "rig:installed-probe",
            "GC_PRIVATE_EVIDENCE_PROBE_TOKEN_FILE": str(token_file),
            "GC_PRIVATE_EVIDENCE_PROBE_WORKSPACE": str(workspace),
            "GC_PRIVATE_EVIDENCE_PROBE_ROOT": str(root),
            "GC_PRIVATE_EVIDENCE_PROBE_BD_BINARY": str(Path(bd).resolve()),
        })
        command = [go, "test", "-tags", "integration", "./internal/attemptevidence", "-run",
                   "^TestInstalledBdPrivateEvidenceAdapter$", "-count=1", "-v"]
        result = subprocess.run(command, cwd=repo, env=test_env, capture_output=True,
                                text=True, timeout=600)
        test_log = result.stdout + result.stderr
        safe_test_log = test_log.replace(token, "[redacted]")
        safe_test_log = safe_test_log.replace("private-attempt-evidence-" + token[:16], "[redacted]")
        safe_test_log = safe_test_log.replace(token[:16], "[redacted]")
        log_path = Path(tempfile.gettempdir()) / f"gc-private-evidence-adapter-{os.getpid()}.log"
        log_path.write_text(safe_test_log[-65536:])
        log_path.chmod(0o600)
        report["go_test_log_path"] = str(log_path)
        if result.returncode:
            report["go_test_exit"] = result.returncode
            report["go_test_tail"] = safe_test_log[-1600:]
            report["passed"] = False
        else:
            report["passed"] = "TestInstalledBdPrivateEvidenceAdapter" in result.stdout and "PASS" in result.stdout
            report["go_test_exit"] = 0
        report["checks"]["context_and_required_routes"] = True
        report["checks"]["gas_city_adapter_test"] = report["passed"]
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=5)
        for pid in owned_proxies(workspace):
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        for _ in range(50):
            if not owned_proxies(workspace):
                break
            time.sleep(0.1)
        report["detached_proxies_terminal"] = not owned_proxies(workspace)
        report["services_terminal"] = all(child.poll() is not None for child in children)
        token_file.unlink(missing_ok=True)
        try:
            remove_fixture(root)
            report["fixture_removed"] = not root.exists()
        except OSError as error:
            report["fixture_removed"] = False
            report["fixture_cleanup_error"] = type(error).__name__
        report["checks"]["cleanup"] = (
            report["detached_proxies_terminal"]
            and report["services_terminal"]
            and report["fixture_removed"]
        )
        report["passed"] = bool(report.get("passed") and report["checks"]["cleanup"])
        report_path = Path(tempfile.gettempdir()) / f"gc-private-evidence-adapter-report-{os.getpid()}.json"
        report["report_path"] = str(report_path)
        report_path.write_text(json.dumps(report, indent=2) + "\n")

    print(json.dumps(report, indent=2))
    return 0 if report.get("passed") else 1


if __name__ == "__main__":
    raise SystemExit(main())
