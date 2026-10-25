#!/usr/bin/env python3
"""Inspect or restore a retained release locally while its Rook service is stopped."""

import argparse
from datetime import datetime, timezone
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from uuid import uuid4


def command(*args):
    return subprocess.check_output(args, text=True, timeout=660).strip()


def identity(value):
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9-]{0,127}", value):
        raise argparse.ArgumentTypeError("use a deployment or retained release ID")
    return value


def read_json(path):
    with path.open() as file:
        return json.load(file)


def atomic_json(path, value):
    descriptor, temporary = tempfile.mkstemp(prefix=".recovery-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as file:
            os.fchmod(file.fileno(), 0o600)
            json.dump(value, file, indent=2)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def containers(snapshot):
    ids = snapshot.get("roleContainers") or [snapshot["containerId"]]
    if not ids or any(not re.fullmatch(r"[0-9a-f]{12,64}", value) for value in ids):
        raise ValueError("retained release contains invalid container IDs")
    return ids


def uncertain_operations(state):
    uncertain = []
    for path in sorted((state / "operations").glob("*.json")):
        result = read_json(path)
        if result.get("status") in ("running", "interrupted"):
            uncertain.append({"id": path.stem, "status": result["status"]})
    return uncertain


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, file, code, message, headers, url):
        return None


def wait_healthy(port, path):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
    deadline = time.monotonic() + 60
    while True:
        try:
            with opener.open(f"http://127.0.0.1:{port}{path}", timeout=3) as response:
                if response.status == 200:
                    return
        except (OSError, urllib.error.URLError):
            pass
        if time.monotonic() >= deadline:
            raise ValueError("retained release did not pass readiness; binding preserved")
        time.sleep(1)


def inspect(state, deployment):
    bindings = read_json(state / "bindings.json")
    current = bindings.get(deployment)
    releases = []
    for path in sorted((state / "releases" / deployment).glob("*.json")):
        release = read_json(path)
        releases.append({
            "releaseId": release.get("releaseId"),
            "imageRef": release.get("imageRef"),
            "containers": containers(release),
            "port": release.get("port"),
            "healthPath": release.get("healthPath"),
        })
    return {"binding": current, "releases": releases,
            "uncertainOperations": uncertain_operations(state),
            "fenced": (state / "recovery-fence").exists()}


def restore(args, state):
    capabilities = json.loads(command(args.rook_binary, "--capabilities"))
    if "host-recovery-fence.v1" not in capabilities:
        raise ValueError("the installed Rook does not support the recovery fence")
    load = command("systemctl", "show", args.rook_service, "--property=LoadState", "--value")
    active = command("systemctl", "show", args.rook_service, "--property=ActiveState", "--value")
    if load != "loaded" or active not in ("inactive", "failed"):
        raise ValueError("stop the configured Rook systemd service before offline recovery")

    # Persist the fence before touching containers. It remains on every failure.
    atomic_json(state / "recovery-fence", {"deploymentId": args.deployment, "releaseId": args.release})
    pending = {entry["id"] for entry in uncertain_operations(state)}
    if pending != set(args.acknowledge_operation):
        raise ValueError("inspect and explicitly acknowledge each uncertain operation: " + ", ".join(sorted(pending)))

    bindings = read_json(state / "bindings.json")
    current = bindings[args.deployment]
    current_snapshot = current["metadata"]
    current_id = current_snapshot.get("releaseId") or current_snapshot["containerId"]
    if current_id != args.expect_current:
        raise ValueError("current release changed; inspect again before restoring")
    target = read_json(state / "releases" / args.deployment / (args.release + ".json"))
    if target["releaseId"] != args.release:
        raise ValueError("retained release identity differs from the requested release")
    path = target.get("healthPath")
    if not isinstance(path, str) or not path.startswith("/") or path.startswith("//"):
        raise ValueError("retained release is missing its verified health path")
    port = target["port"]
    if not isinstance(port, int) or not 1 <= port <= 65535:
        raise ValueError("retained release has an invalid local port")

    # Inspect every image and container before starting any of them. No rebuild,
    # image pull, secret lookup, database command, or new container is permitted.
    target_ids = containers(target)
    stopped_ids = []
    for container in target_ids:
        image = command("docker", "inspect", "--format", "{{.Image}}", container)
        command("docker", "image", "inspect", "--format", "{{.Id}}", image)
        if command("docker", "inspect", "--format", "{{.State.Running}}", container) == "false":
            stopped_ids.append(container)
    try:
        command("docker", "start", *target_ids)
        mappings = json.loads(command("docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", target["containerId"]))
        local_ports = [int(mapping["HostPort"]) for entries in mappings.values() for mapping in (entries or [])
                       if mapping.get("HostIp") == "127.0.0.1"]
        if len(local_ports) != 1 or not 1 <= local_ports[0] <= 65535:
            raise ValueError("retained web process does not have exactly one loopback port")
        port = local_ports[0]
        wait_healthy(port, path)
        for container in target_ids:
            status = command("docker", "inspect", "--format", "{{.State.Running}} {{.State.Restarting}}", container)
            if status != "true false":
                raise ValueError("retained process is not running stably; binding preserved")
    except (OSError, ValueError, subprocess.SubprocessError):
        if stopped_ids:
            command("docker", "stop", "--time", "30", *stopped_ids)
        raise

    target["port"] = port
    verification = {"id": str(uuid4()), "releaseId": args.release, "port": port,
                    "verifiedAt": datetime.now(timezone.utc).isoformat()}
    target["recoveryVerification"] = verification
    if current_id != args.release:
        target["previousReleaseId"] = current_id
        target["previousContainerId"] = current_snapshot["containerId"]
    atomic_json(state / "releases" / args.deployment / (args.release + ".json"), target)
    bindings[args.deployment] = {"port": port, "metadata": target}
    atomic_json(state / "bindings.json", bindings)
    old_ids = [value for value in containers(current_snapshot) if value not in target_ids]
    if old_ids:
        seconds = current_snapshot.get("shutdownSeconds", 60)
        if not isinstance(seconds, int) or not 1 <= seconds <= 600:
            raise ValueError("invalid shutdown timeout; restored binding saved, prior processes retained")
        command("docker", "stop", "--time", str(seconds), *old_ids)
    return {"restoredReleaseId": args.release, "localPort": port, "fenced": True,
            "verification": verification,
            "rookServiceStarted": False, "databaseRestored": False,
            "next": "Verify workflows locally; start Rook when its authentication service is available. Reconcile control-plane state and uncertain operations before removing recovery-fence."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", required=True, type=Path)
    parser.add_argument("--deployment", required=True, type=identity)
    actions = parser.add_subparsers(dest="action", required=True)
    actions.add_parser("inspect")
    recovery = actions.add_parser("restore")
    recovery.add_argument("--release", required=True, type=identity)
    recovery.add_argument("--expect-current", required=True, type=identity)
    recovery.add_argument("--rook-service", required=True)
    recovery.add_argument("--rook-binary", default="/usr/local/bin/rook")
    recovery.add_argument("--acknowledge-operation", action="append", default=[])
    args = parser.parse_args()
    state = args.state_dir.resolve(strict=True)
    if args.action == "inspect":
        result = inspect(state, args.deployment)
    else:
        with (state / "offline-recovery.lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = restore(args, state)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"Recovery stopped: {error}", file=sys.stderr)
        sys.exit(1)
