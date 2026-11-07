#!/usr/bin/env python3
"""Apply a reviewed local provider plan through the existing restart worker."""
import argparse
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import subprocess
import sys
import tarfile
import time
import tomllib

sys.dont_write_bytecode = True


def module_at(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def fingerprint(path):
    path = Path(path)
    if path.is_symlink():
        return "link:" + os.readlink(path)
    if not path.exists():
        return None
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_file(path, content, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(path.name + ".provider-update")
    with temp.open("wb") as stream:
        os.chmod(temp, mode)
        stream.write(content)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temp, path)


def migrate_bindings(directory, lookup):
    mapping = {"aidp": "AIDP", "AIDP": "AIDP", "csj": "AIDP", "OpenAI": "openai", "openai": "openai", "Seed": "maso", "code_router": "maso", "maso": "maso"}
    changed = {}
    for path in Path(directory).glob("codex*.json"):
        data = json.loads(path.read_text())
        rows = {}
        for sid, session in (data.get("sessions") or {}).items():
            if not session:
                continue
            old = session.get("active_provider")
            owner = old or lookup(session.get("agent_session_id", ""))
            target = mapping.get(owner)
            if target and (old != target or not session.get("provider_bound")):
                rows[sid] = {"active_provider": target, "provider_bound": True}
                session.update(rows[sid])
        if rows:
            write_file(path, (json.dumps(data, ensure_ascii=False, indent=2) + "\n").encode())
            changed[path.name] = rows
    return changed


def patch_worker(worker, manifest, plan, launcher):
    original_verify = worker.verify_sessions
    original_start = worker.base.START
    original_check = worker.check
    archive = Path(plan["archive"])
    rollback = archive / "rollback.tar.gz"

    def check(state):
        value = original_check(state)
        if state["phase"] in ("prepared", "armed", "waiting_for_reply", "waiting_for_reply_and_idle"):
            for item in plan["files"]:
                worker.require(fingerprint(item["target"]) == item["before"], "Configuration changed after preparation")
                if "source" in item:
                    worker.require(fingerprint(item["source"]) == item["sha256"], "Staged file changed after preparation")
            for item in plan["old_processes"]:
                worker.require(worker.alive(item["pid"], item["start"]), "Provider service identity changed after preparation")
        return value

    def stop_old_services():
        for item in plan["old_processes"]:
            if worker.alive(item["pid"], item["start"]):
                os.kill(item["pid"], signal.SIGTERM)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if all(not worker.alive(i["pid"], i["start"]) for i in plan["old_processes"]):
                return
            time.sleep(.2)
        raise RuntimeError("An old provider service did not stop")

    def start_old_services():
        subprocess.run(["bash", str(Path.home() / ".csj/start-csj-proxy.sh")], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run([str(Path.home() / ".local/bin/codex-sd"), "--bridge-status"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run([str(Path.home() / ".local/bin/ensure-codex-proxies")], check=True)

    def backup():
        if rollback.exists():
            raise RuntimeError("Rollback archive already exists")
        roots = [Path(x) for x in plan["retire_paths"]]
        paths = roots + [Path(x["target"]) for x in plan["files"] if x["before"] is not None and not any(Path(x["target"]).is_relative_to(r) for r in roots)]
        with tarfile.open(rollback, "w:gz", compresslevel=1) as tar:
            for path in paths:
                if path.exists() or path.is_symlink():
                    tar.add(path, arcname=str(path).lstrip("/"), recursive=True)
        rollback.chmod(0o600)
        worker.record(manifest, rollback_archive_ready=True)

    def restore():
        current = json.loads(manifest.read_text())
        if not current.get("rollback_archive_ready"):
            start_old_services()
            return
        # New services are owned by the named tmux sessions in this plan.
        for service in plan["new_services"]:
            result = subprocess.run(["tmux", "list-panes", "-t", "=" + service["session"], "-F", "#{pane_pid}"], capture_output=True, text=True)
            for value in result.stdout.splitlines():
                if value.isdigit() and worker.alive(int(value)):
                    os.kill(int(value), signal.SIGTERM)
        time.sleep(1)
        for item in plan["files"]:
            # Remove installed symlinks before extracting old regular files;
            # otherwise tar could write through them into the new launcher.
            path = Path(item["target"])
            if path.is_symlink() or path.is_file():
                path.unlink()
        with tarfile.open(rollback) as tar:
            for member in tar.getmembers():
                p = Path(member.name)
                if p.is_absolute() or ".." in p.parts:
                    raise RuntimeError("Invalid rollback archive member")
            tar.extractall("/")
        for path in (manifest.parent / "session-files.before").glob("codex*.json"):
            write_file(worker.SESSIONS / path.name, path.read_bytes())
        worker.base.START = original_start
        worker.record(manifest, configuration_applied=False)
        start_old_services()

    def install(state, rollback=False):
        if rollback:
            restore()
            return
        stop_old_services()
        backup()
        for item in plan["files"]:
            target = Path(item["target"])
            if "link" in item:
                target.parent.mkdir(parents=True, exist_ok=True)
                temp = target.with_name(target.name + ".provider-update")
                temp.symlink_to(item["link"])
                os.replace(temp, target)
            else:
                content = Path(item["source"]).read_bytes()
                worker.require(hashlib.sha256(content).hexdigest() == item["sha256"], "Candidate file changed")
                write_file(target, content, item.get("mode", 0o600))
        changes = migrate_bindings(worker.SESSIONS, lambda sid: launcher.saved_provider(Path.home() / ".codex", sid))
        worker.record(manifest, binding_migration=changes, configuration_applied=True)
        removed = []
        for name in plan["retire_paths"]:
            path = Path(name)
            if path.is_symlink() or path.is_file():
                path.unlink()
            elif path.is_dir():
                shutil.rmtree(path)
            removed.append(name)
        worker.record(manifest, retired_provider_paths=removed)
        worker.base.START = plan["start_script"]
        subprocess.run([str(Path.home() / ".local/bin/ensure-codex-proxies")], check=True)

    def verify(before, directory=worker.SESSIONS):
        current = json.loads(manifest.read_text())
        expected = copy.deepcopy(before)
        if current.get("configuration_applied"):
            for filename, rows in current.get("binding_migration", {}).items():
                for sid, fields in rows.items():
                    expected[filename]["sessions"][sid].update(fields)
            native = tomllib.loads((Path.home() / ".codex/config.toml").read_text())
            worker.require(set(native["model_providers"]) == {"AIDP", "OpenAI", "maso"}, "Unexpected native providers")
            worker.require(native["model_provider"] == "AIDP" and native["sandbox_mode"] == "danger-full-access", "Native defaults differ")
            cc = tomllib.loads((Path.home() / ".cc-connect/config.toml").read_text())
            agent = next(p["agent"] for p in cc["projects"] if p["agent"]["type"] == "codex")
            worker.require([p["name"] for p in agent["providers"]] == ["AIDP", "openai", "maso"] and agent["options"]["provider"] == "AIDP", "Unexpected cc-connect providers")
            subprocess.run([str(Path.home() / ".local/bin/ensure-codex-proxies"), "--check"], check=True)
        original_verify(expected, directory)

    def sync(state, path):
        worker.record(path, provider_configuration_verified=True,
                      proxy_processes_unchanged=False, provider_services_reconfigured=True)
        worker.base.atomic_json(Path.home() / ".local/share/codex-provider/state.json", {
            "providers": ["AIDP", "openai", "maso"], "default": "AIDP", "sandbox_mode": "danger-full-access",
            "configuration_commit": plan.get("configuration_commit"), "deployment_manifest": str(path)})

    worker.check, worker.install_config, worker.verify_sessions = check, install, verify
    worker.sync_activation_state = sync


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--arm", action="store_true")
    parser.add_argument("--run", action="store_true")
    args = parser.parse_args()
    manifest = args.manifest.resolve()
    plan = json.loads((manifest.parent / "plan.json").read_text())
    worker = module_at("restart_worker", plan["worker_helper"])
    launcher = module_at("launcher", manifest.parent / "staged/codex_launcher.py")
    patch_worker(worker, manifest, plan, launcher)
    if args.arm:
        state = json.loads(manifest.read_text())
        worker.require(state["phase"] == "prepared", "Deployment is not prepared")
        worker.check(state)
        exists = subprocess.run(["tmux", "has-session", "-t", "=" + state["worker_tmux"]], capture_output=True)
        worker.require(exists.returncode != 0, "Deployment worker already exists")
        worker.record(manifest, phase="armed", armed_log=worker.log_identity())
        command = shlex.join(["/usr/bin/python3", "-B", str(Path(__file__).resolve()), "--manifest", str(manifest), "--run"])
        command += " >> " + shlex.quote(str(manifest.parent / "worker.log")) + " 2>&1"
        subprocess.run(["tmux", "new-session", "-d", "-s", state["worker_tmux"], command], check=True)
        print("Provider reconfiguration armed")
    elif args.run:
        try:
            worker.run_worker(manifest)
        except Exception as error:
            worker.recover(manifest, error)
            raise SystemExit(1)
    else:
        print(json.dumps(worker.check(json.loads(manifest.read_text()))))


if __name__ == "__main__":
    os.umask(0o077)
    main()
