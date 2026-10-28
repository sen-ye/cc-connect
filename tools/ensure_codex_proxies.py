#!/usr/bin/env python3
"""Start configured loopback Codex relays in tmux without inheriting API keys."""
import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import time
import urllib.request


def healthy(service):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(service["url"] + "/_cc_connect_health", timeout=0.5) as reply:
            data = json.load(reply)
            return (reply.status == 200 and data.get("ok") is True
                    and data.get("service") == "codex-provider"
                    and data.get("provider") == service["name"])
    except (OSError, ValueError):
        return False


def ensure_service(binary, service, check_only=False):
    if healthy(service):
        return
    if check_only:
        raise RuntimeError("Codex relay is not ready: " + service["name"])
    target = "=" + service["session"]
    exists = subprocess.run(["tmux", "has-session", "-t", target],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    if not exists:
        # tmux can retain an old server environment. Clear it inside the pane,
        # not merely in the environment of the tmux client process.
        command = ["env", "-i", "HOME=" + str(Path.home()),
                   "PATH=" + os.defpath, binary, "--config", service["config"],
                   "--listen", service["listen"]]
        log = Path(service["log"])
        log.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        fd = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        os.close(fd)
        launch = "exec " + shlex.join(command) + " >> " + shlex.quote(str(log)) + " 2>&1"
        subprocess.run(["tmux", "new-session", "-d", "-s", service["session"], launch],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    # A concurrent launcher may have won new-session; wait for that instance.
    for _ in range(30):
        if healthy(service):
            return
        time.sleep(0.1)
    # Never kill or replace an unknown listener/session, nor route to a fallback.
    raise RuntimeError("Codex relay did not become ready: " + service["name"]
                       + "; inspect " + service["log"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path,
                        default=Path.home() / ".config/codex-provider/services.json")
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    try:
        config = json.loads(args.config.read_text())
        for service in config["services"]:
            ensure_service(config["binary"], service, args.check)
    except (OSError, ValueError, KeyError, RuntimeError) as error:
        # Do not echo configuration contents or a subprocess environment.
        message = str(error) if isinstance(error, RuntimeError) else "Cannot read/start configured Codex relays"
        parser.exit(1, message + "\n")


if __name__ == "__main__":
    main()
