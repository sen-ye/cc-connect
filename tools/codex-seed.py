#!/usr/bin/env python3
"""Launch a configured Codex provider without putting its key in argv or TOML."""
import json
import os
from pathlib import Path
import sys


def router_key(home, environment):
    for name in ("CC_CODE_ROUTER_API_KEY", "SEED_CODE_ROUTER_API_KEY"):
        if environment.get(name, "").strip():
            return environment[name].strip()
    try:
        value = json.loads((home / ".codex/code-router-auth.json").read_text())["api_key"]
        if isinstance(value, str) and value.strip():
            return value.strip()
    except (OSError, ValueError, KeyError, TypeError):
        pass
    raise ValueError("codex-seed: Seed key missing; set CC_CODE_ROUTER_API_KEY or configure ~/.codex/code-router-auth.json")


def main():
    home = Path.home()
    try:
        os.environ["CC_CODE_ROUTER_API_KEY"] = router_key(home, os.environ)
    except ValueError as error:
        print(error, file=sys.stderr)
        return 1
    binary = home / ".local/share/codex-seed/codex-provider"
    config = home / ".config/codex-seed/provider.toml"
    try:
        os.execv(binary, [str(binary), "--config", str(config), "--", *sys.argv[1:]])
    except OSError:
        print("codex-seed: launcher installation is missing or not executable", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
