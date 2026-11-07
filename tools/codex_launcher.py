#!/usr/bin/env python3
"""Shared provider launcher and cc-connect stdio adapter; credentials stay in env."""
import copy
import json
import os
from pathlib import Path
import queue
import re
import signal
import sqlite3
import subprocess
import sys
import threading
import tomllib

LAUNCHERS = {"codex-aidp": "AIDP", "codex-openai": "openai", "codex-maso": "maso"}
# Lowercase openai is a reserved built-in ID in the native CLI.
NATIVE_IDS = {"AIDP": "AIDP", "openai": "OpenAI", "maso": "maso"}
ALIASES = {"AIDP": ("aidp", "csj"), "openai": ("OpenAI",), "maso": ("Seed", "code_router")}
KEY_ENV = {"AIDP": "AIDP_API_KEY", "openai": "OPENAI_OFFICIAL_API_KEY", "maso": "MASO_API_KEY"}
UUID = re.compile(r"^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$", re.I)
VALUE_OPTIONS = {"-c", "--config", "-m", "--model", "-s", "--sandbox", "-a", "--ask-for-approval", "-C", "--cd", "-i", "--image", "--add-dir", "--enable", "--disable", "-o", "--output-last-message", "--output-schema", "--listen"}


def canonical(name):
    for provider, aliases in ALIASES.items():
        if name == provider or name in aliases:
            return provider
    return None


def inline_toml(value):
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, list):
        return "[" + ", ".join(inline_toml(v) for v in value) + "]"
    if isinstance(value, dict):
        return "{" + ", ".join(json.dumps(k) + "=" + inline_toml(v) for k, v in value.items()) + "}"
    raise ValueError("Unsupported configuration value")


def merge(base, values):
    for key, value in values.items():
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            merge(base[key], value)
        else:
            base[key] = copy.deepcopy(value)


def options(args):
    overrides, command, resume = {}, "", ""
    explicit_sandbox = False
    i = 0
    while i < len(args):
        arg = args[i]
        if arg == "--":
            break
        key, sep, attached = arg.partition("=")
        if key in ("-s", "--sandbox", "--dangerously-bypass-approvals-and-sandbox"):
            explicit_sandbox = True
        if key in ("-c", "--config"):
            if not sep:
                i += 1
                if i >= len(args):
                    raise ValueError("Missing configuration override")
                attached = args[i]
            try:
                value = tomllib.loads(attached)
            except tomllib.TOMLDecodeError:
                raise ValueError("Invalid configuration override") from None
            merge(overrides, value)
            explicit_sandbox |= "sandbox_mode" in value
        elif key in VALUE_OPTIONS and not sep:
            i += 1
        elif not arg.startswith("-"):
            if arg in ("exec", "e") and not command:
                pass
            elif not command:
                command = arg
                if arg not in ("resume", "fork", "app-server"):
                    break
            elif command in ("resume", "fork") and UUID.fullmatch(arg):
                resume = arg
                break
            else:
                break
        i += 1
    return overrides, command, resume, explicit_sandbox


def saved_provider(native_home, thread):
    if not UUID.fullmatch(thread or ""):
        return None
    db = native_home / "state_5.sqlite"
    if db.is_file():
        try:
            with sqlite3.connect(f"file:{db}?mode=ro", uri=True, timeout=2) as conn:
                row = conn.execute("SELECT model_provider FROM threads WHERE id=?", (thread,)).fetchone()
                if row and row[0]:
                    return row[0]
        except sqlite3.Error:
            pass
    for folder in ("sessions", "archived_sessions"):
        for path in (native_home / folder).glob("**/*" + thread + ".jsonl"):
            try:
                with path.open() as stream:
                    first = json.loads(stream.readline())
                if first.get("type") == "session_meta":
                    return first.get("payload", {}).get("model_provider")
            except (OSError, ValueError):
                continue
    return None


def rewrite_request(line, provider, lookup):
    try:
        request = json.loads(line)
    except ValueError:
        return line
    params = request.get("params") or {}
    if request.get("method") == "thread/start" and canonical(params.get("modelProvider")) == provider:
        params["modelProvider"] = NATIVE_IDS[provider]
        return json.dumps(request, separators=(",", ":"))
    if request.get("method") not in ("thread/resume", "turn/start") or not params.get("threadId"):
        return line
    saved = lookup(params["threadId"])
    if saved and canonical(saved) == provider:
        if request["method"] == "thread/resume" or "modelProvider" in params:
            params["modelProvider"] = saved
            return json.dumps(request, separators=(",", ":"))
    return line


def load_keys(directory, environment):
    try:
        sources = json.loads((directory / "launcher.json").read_text())["credential_files"]
        credentials = {}
        for provider in KEY_ENV:
            source = sources[provider]
            credentials[provider] = json.loads(Path(source["path"]).read_text())[source["field"]]
            if not isinstance(credentials[provider], str) or not credentials[provider].strip():
                raise ValueError()
    except (OSError, ValueError, KeyError, TypeError):
        raise ValueError("Codex provider credentials are missing or invalid") from None
    result = environment.copy()
    for provider, name in KEY_ENV.items():
        result[name] = credentials[provider].strip()
    return result


def affinity(native_home, environment, thread):
    if environment.get("AIDP_EXTRA"):
        return
    sid = environment.get("AIDP_SESSION_ID")
    try:
        mapping = json.loads((native_home / "aidp-session-ids.json").read_text())
        sid = sid or mapping.get(thread)
    except (OSError, ValueError):
        pass
    try:
        sid = sid or (native_home / "aidp-sticky-session-id").read_text().strip()
    except OSError:
        pass
    if sid:
        environment["AIDP_SESSION_ID"] = str(sid)
        environment["AIDP_EXTRA"] = json.dumps({"session_id": str(sid)})
        environment.setdefault("AIDP_LOGID", "codex-" + str(sid))


def build_launch(args, selected, config, settings, environment, lookup):
    overrides, command, thread, explicit_sandbox = options(args)
    effective = copy.deepcopy(config)
    merge(effective, overrides)
    requested = effective.get("model_provider", "AIDP")
    provider = selected or canonical(requested)
    if provider not in KEY_ENV:
        raise ValueError("Use codex-aidp, codex-openai or codex-maso")
    if selected and "model_provider" in overrides and canonical(requested) != selected:
        raise ValueError("Use the matching launcher to select another provider")
    if selected and any(a in ("-p", "--profile", "--remote", "--oss", "--local-provider") or a.startswith(("--profile=", "--remote=", "--local-provider=")) or (a.startswith("-p") and not a.startswith("--")) for a in args):
        raise ValueError("Provider launchers do not accept routing/profile overrides")
    definitions = effective.get("model_providers", {})
    native_id = NATIVE_IDS[provider]
    definition = copy.deepcopy(overrides.get("model_providers", {}).get(requested) or definitions.get(native_id))
    if not definition:
        raise ValueError("Selected provider is not configured")
    native_provider = native_id
    if thread:
        saved = lookup(thread)
        if saved and canonical(saved) != provider:
            raise ValueError("This conversation belongs to a different or retired provider")
        native_provider = saved or native_id
    table = {native_id: definition}
    for alias in ALIASES[provider]:
        table[alias] = copy.deepcopy(definition)
    prefix = ["-c", "model_providers=" + inline_toml(table), "-c", "model_provider=" + inline_toml(native_provider)]
    catalog = settings.get("model_catalogs", {}).get(provider)
    if catalog and "model_catalog_json" not in overrides:
        prefix += ["-c", "model_catalog_json=" + inline_toml(catalog)]
    if selected:
        if command != "app-server" and "--no-daemon" not in args:
            prefix.append("--no-daemon")
        if not explicit_sandbox:
            # The config override also covers exec resume, whose subcommand
            # does not accept its own --sandbox option.
            if command != "app-server":
                prefix += ["--sandbox", "danger-full-access"]
            prefix += ["-c", 'sandbox_mode="danger-full-access"']
    env = environment.copy()
    if "CC_CODE_ROUTER_API_KEY" in str(definition):
        env["CC_CODE_ROUTER_API_KEY"] = env.get("MASO_API_KEY", "")
    for name in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"):
        env.pop(name, None)
    if provider == "openai":
        env["http_proxy"] = env["https_proxy"] = settings["openai_proxy"]
    env["NO_PROXY"] = env["no_proxy"] = "127.0.0.1,localhost,::1"
    # Host-provided options precede the resolved table, so transient aliases
    # use the host's per-session proxy and never bypass it.
    if command == "app-server":
        native_args = args + prefix
    else:
        native_args = prefix + args
    return provider, native_args, env, command == "app-server" and not any(a == "--listen" or a.startswith("--listen=") for a in args)


def serve_stdio(binary, args, environment, provider, lookup):
    """Match the native process to the first resumed thread before attaching it.

    Codex checks its process-level provider before applying resume overrides.
    cc-connect owns one thread per stdio process, so an alias can be selected
    after initialize by restarting the still-unattached server and replaying
    its handshake. No task, thread/start, or model request is replayed.
    """
    child = None
    forward = None
    output_thread = None
    initialization = None
    initialized = None
    attached = False
    native_id = NATIVE_IDS[provider]

    def spawn(selected, replay=False):
        nonlocal child, forward, output_thread
        argv = [binary, *args, "-c", "model_provider=" + inline_toml(selected)]
        child = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, env=environment, text=True)
        process = child
        allowed = threading.Event()
        allowed.set()
        forward = allowed
        ready = threading.Event()
        handshake_ok = []
        def output():
            try:
                for line in process.stdout:
                    response = json.loads(line)
                    if replay and response.get("id") == initialization.get("id"):
                        handshake_ok.append("error" not in response)
                        ready.set()
                    elif allowed.is_set():
                        sys.stdout.write(line)
                        sys.stdout.flush()
            except (OSError, ValueError):
                pass
            finally:
                process.stdout.close()
        output_thread = threading.Thread(target=output, daemon=True)
        output_thread.start()
        if replay:
            process.stdin.write(json.dumps(initialization) + "\n")
            process.stdin.flush()
            if not ready.wait(15) or not handshake_ok[0]:
                raise ValueError("Could not initialize the historical provider")
            if initialized is not None:
                process.stdin.write(json.dumps(initialized) + "\n")
                process.stdin.flush()

    def stop():
        forward.clear()
        child.stdin.close()
        try:
            child.wait(timeout=5)
        except subprocess.TimeoutExpired:
            child.terminate()
            child.wait(timeout=5)
        output_thread.join(timeout=1)

    spawn(native_id)
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda number, frame: child.send_signal(number) if child.poll() is None else None)
    incoming = queue.Queue()
    def input_lines():
        pending = b""
        while chunk := os.read(sys.stdin.fileno(), 65536):
            pending += chunk
            while b"\n" in pending:
                line, pending = pending.split(b"\n", 1)
                incoming.put(line.decode() + "\n")
        if pending:
            incoming.put(pending.decode())
        incoming.put(None)
    threading.Thread(target=input_lines, daemon=True).start()
    try:
        while True:
            try:
                line = incoming.get(timeout=.2)
            except queue.Empty:
                if child.poll() is not None:
                    output_thread.join(timeout=1)
                    return child.returncode
                continue
            if line is None:
                break
            request = json.loads(line)
            method = request.get("method")
            if method == "initialize":
                initialization = request
            elif method == "initialized":
                initialized = request
            elif method in ("thread/start", "thread/resume"):
                saved = lookup((request.get("params") or {}).get("threadId", "")) if method == "thread/resume" else None
                if not attached and saved and canonical(saved) == provider and saved != native_id:
                    if initialization is None:
                        raise ValueError("Initialize is required before resuming a thread")
                    stop()
                    native_id = saved
                    spawn(native_id, replay=True)
                attached = True
            child.stdin.write(rewrite_request(line, provider, lookup).rstrip("\n") + "\n")
            child.stdin.flush()
        child.stdin.close()
        result = child.wait()
        output_thread.join(timeout=1)
        return result
    finally:
        if child.poll() is None:
            child.terminate()
            child.wait(timeout=5)


def main():
    directory = Path(os.environ.get("CODEX_PROVIDER_CONFIG_DIR", str(Path.home() / ".config/codex-provider")))
    native_home = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex")))
    args = sys.argv[1:]
    if args and args[0] == "--with-keys":
        if len(args) < 2:
            raise ValueError("A command is required after --with-keys")
        env = load_keys(directory, os.environ)
        os.execvpe(args[1], args[1:], env)
    settings = json.loads((directory / "launcher.json").read_text())
    config = tomllib.loads((native_home / "config.toml").read_text())
    selected = LAUNCHERS.get(Path(sys.argv[0]).name)
    lookup = lambda thread: saved_provider(native_home, thread)
    env = load_keys(directory, os.environ)
    provider, native_args, env, stdio = build_launch(args, selected, config, settings, env, lookup)
    _, _, thread, _ = options(args)
    if selected and not env.get("AIDP_EXTRA") and not env.get("AIDP_SESSION_ID"):
        env.update(settings.get("affinity_defaults", {}).get(selected, {}))
    if provider in ("AIDP", "maso"):
        affinity(native_home, env, thread)
    help_only = any(a in ("--help", "-h", "--version", "-V") for a in args)
    if not help_only and settings.get("ensure_relays"):
        subprocess.run([settings["ensure_relays"], "--provider", provider], check=True)
    if not stdio:
        os.execvpe(settings["native_binary"], [settings["native_binary"], *native_args], env)
    return serve_stdio(settings["native_binary"], native_args, env, provider, lookup)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, subprocess.SubprocessError):
        sys.exit("Codex launcher failed; check the selected provider configuration and relay status.")
