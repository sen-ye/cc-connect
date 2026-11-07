import importlib.util
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

spec = importlib.util.spec_from_file_location("launcher", Path(__file__).with_name("codex_launcher.py"))
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


def fixture():
    return {"model_provider": "AIDP", "model_providers": {
        launcher.NATIVE_IDS[p]: {"name": p, "base_url": "http://127.0.0.1:1234/" + p,
            "env_key": name, "wire_api": "responses", "requires_openai_auth": False}
        for p, name in launcher.KEY_ENV.items()}}


class LauncherTests(unittest.TestCase):
    def test_three_launchers_default_to_requested_sandbox_without_bypassing_approval(self):
        for public, provider in launcher.LAUNCHERS.items():
            with self.subTest(public=public):
                p, args, env, stdio = launcher.build_launch([], provider, fixture(), {"openai_proxy": "http://proxy:8118"}, {"https_proxy": "http://stale"}, lambda _: None)
                self.assertEqual(p, provider)
                self.assertEqual(args[args.index("--sandbox") + 1], "danger-full-access")
                self.assertIn("--no-daemon", args)
                self.assertNotIn("--dangerously-bypass-approvals-and-sandbox", args)
                self.assertNotIn("approval_policy", " ".join(args))
                self.assertEqual(env.get("https_proxy"), "http://proxy:8118" if provider == "openai" else None)
                self.assertFalse(stdio)

    def test_explicit_sandbox_is_not_overridden(self):
        for args in [["--sandbox", "read-only"], ["--sandbox=read-only"], ["-c", 'sandbox_mode="read-only"']]:
            _, actual, _, _ = launcher.build_launch(args, "AIDP", fixture(), {}, {}, lambda _: None)
            self.assertNotIn("danger-full-access", " ".join(actual))

    def test_historical_alias_uses_current_endpoint_and_original_native_id(self):
        thread = "01234567-89ab-cdef-0123-456789abcdef"
        for provider, aliases in launcher.ALIASES.items():
            for alias in aliases:
                with self.subTest(alias=alias):
                    _, args, _, _ = launcher.build_launch(["resume", thread], provider, fixture(), {"openai_proxy": "http://proxy"}, {}, lambda _: alias)
                    self.assertIn('model_provider="' + alias + '"', args)
                    table = launcher.options(args)[0]["model_providers"]
                    self.assertEqual(table[alias]["base_url"], fixture()["model_providers"][launcher.NATIVE_IDS[provider]]["base_url"])

    def test_resume_id_is_found_after_subcommand_flags(self):
        thread = '01234567-89ab-cdef-0123-456789abcdef'
        _, args, _, _ = launcher.build_launch(['exec', 'resume', '--json', '--skip-git-repo-check', thread, 'continue'], 'AIDP', fixture(), {}, {}, lambda _: 'csj')
        self.assertIn('model_provider="csj"', args)

    def test_resume_cannot_change_account_family(self):
        with self.assertRaises(ValueError):
            launcher.build_launch(["resume", "01234567-89ab-cdef-0123-456789abcdef"], "AIDP", fixture(), {}, {}, lambda _: "OpenAI")

    def test_cc_runtime_keeps_per_session_proxy_and_rewrites_resume(self):
        original = ["app-server", "-c", 'model_provider="openai"', "-c", 'model_providers={openai={name="openai",base_url="http://127.0.0.1:4567",env_key="SESSION_KEY",wire_api="responses"}}']
        p, args, _, stdio = launcher.build_launch(original, None, fixture(), {"openai_proxy": "http://proxy"}, {}, lambda _: "OpenAI")
        self.assertTrue(stdio)
        config = launcher.options(args)[0]
        self.assertEqual(config["model_providers"]["OpenAI"]["base_url"], "http://127.0.0.1:4567")
        self.assertEqual(config["model_providers"]["OpenAI"]["env_key"], "SESSION_KEY")
        request = {"id": 1, "method": "thread/resume", "params": {"threadId": "thread", "modelProvider": "openai", "model": "chosen"}}
        result = json.loads(launcher.rewrite_request(json.dumps(request), p, lambda _: "OpenAI"))
        self.assertEqual(result["params"], {"threadId": "thread", "modelProvider": "OpenAI", "model": "chosen"})
        request["method"] = "thread/start"
        self.assertEqual(json.loads(launcher.rewrite_request(json.dumps(request), p, lambda _: "OpenAI"))['params']['modelProvider'], 'OpenAI')

    def test_cc_old_provider_override_keeps_its_proxy_during_cutover(self):
        args = ['app-server', '-c', 'model_provider="Seed"', '-c', 'model_providers={Seed={name="Seed",base_url="http://127.0.0.1:4567",env_key="CC_CODE_ROUTER_API_KEY"}}']
        p, actual, env, _ = launcher.build_launch(args, None, fixture(), {}, {'MASO_API_KEY': 'test'}, lambda _: None)
        self.assertEqual(p, 'maso')
        self.assertEqual(launcher.options(actual)[0]['model_providers']['Seed']['base_url'], 'http://127.0.0.1:4567')
        self.assertEqual(env['CC_CODE_ROUTER_API_KEY'], 'test')

    def test_dedicated_launcher_rejects_retired_profile(self):
        for args in [['-psd'], ['--profile=sd'], ['--local-provider=ollama']]:
            with self.assertRaises(ValueError):
                launcher.build_launch(args, 'AIDP', fixture(), {}, {}, lambda _: None)

    def test_prompt_words_do_not_trigger_resume_and_affinity_stays_stable(self):
        self.assertEqual(launcher.options(["-m", "model", "explain resume", "01234567-89ab-cdef-0123-456789abcdef"])[2], "")
        with tempfile.TemporaryDirectory() as directory:
            native = Path(directory)
            (native / "aidp-session-ids.json").write_text(json.dumps({"thread": "12345678901234"}))
            env = {}
            launcher.affinity(native, env, "thread")
            self.assertEqual(json.loads(env["AIDP_EXTRA"])["session_id"], "12345678901234")
            supplied = {"AIDP_EXTRA": '{"session_id":"existing"}'}
            launcher.affinity(native, supplied, "thread")
            self.assertEqual(supplied["AIDP_EXTRA"], '{"session_id":"existing"}')

    def test_credentials_are_loaded_from_dedicated_files_and_errors_are_redacted(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            refs = {}
            for provider in launcher.KEY_ENV:
                p = root / (provider + ".json")
                p.write_text(json.dumps({"key": "test-" + provider}))
                refs[provider] = {"path": str(p), "field": "key"}
            (root / "launcher.json").write_text(json.dumps({"credential_files": refs}))
            env = launcher.load_keys(root, {})
            self.assertEqual(env["MASO_API_KEY"], "test-maso")
            p.write_text('{"key": 12345}')
            with self.assertRaises(ValueError) as error:
                launcher.load_keys(root, {})
            self.assertNotIn("12345", str(error.exception))

    def test_stdio_native_exit_does_not_leave_bridge_waiting_on_input(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            native = root / 'native'
            native.write_text('#!/bin/sh\nexit 42\n')
            native.chmod(0o755)
            credential = root / 'key.json'
            credential.write_text('{"key":"test-only"}')
            (root / 'launcher.json').write_text(json.dumps({
                'native_binary': str(native),
                'credential_files': {p: {'path': str(credential), 'field': 'key'} for p in launcher.KEY_ENV}}))
            (root / 'config.toml').write_text('\n'.join(k + '=' + launcher.inline_toml(v) for k, v in fixture().items()))
            env = dict(os.environ, CODEX_HOME=str(root), CODEX_PROVIDER_CONFIG_DIR=str(root))
            script = Path(__file__).with_name('codex_launcher.py')
            process = subprocess.Popen([sys.executable, str(script), 'app-server'], env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                self.assertEqual(process.wait(timeout=5), 42)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
                process.stdin.close()
                process.stdout.close()
                process.stderr.close()


@unittest.skipUnless(os.environ.get('CC_CODEX_NATIVE_TEST_BIN'), 'opt-in native Codex with a local fake API')
class NativeLauncherTests(unittest.TestCase):
    def test_native_new_resume_and_cc_stdio_keep_provider_and_sandbox(self):
        requests = []
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"models":[]}')

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                requests.append((self.path, self.headers.get('Authorization'), body))
                count = len(requests)
                item = {'id': f'msg_{count}', 'type': 'message', 'role': 'assistant', 'status': 'completed', 'phase': 'final_answer', 'content': [{'type': 'output_text', 'text': 'fixture-ok', 'annotations': []}]}
                response = {'id': f'resp_{count}', 'object': 'response', 'model': 'gpt-6-astra', 'status': 'completed', 'output': [item], 'usage': {'input_tokens': 10, 'output_tokens': 4, 'total_tokens': 14, 'input_tokens_details': {'cached_tokens': 0}, 'output_tokens_details': {'reasoning_tokens': 0}}}
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                for kind, fields in [('response.created', {'response': dict(response, status='in_progress', output=[])}), ('response.output_item.done', {'output_index': 0, 'item': item}), ('response.completed', {'response': response})]:
                    self.wfile.write(('event: ' + kind + '\ndata: ' + json.dumps(dict(fields, type=kind)) + '\n\n').encode())

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
            self.addCleanup(server.server_close)
            threading.Thread(target=server.serve_forever, daemon=True).start()
            self.addCleanup(server.shutdown)
            config = fixture()
            config.update(model='gpt-6-astra', sandbox_mode='danger-full-access', approval_policy='never')
            for p, definition in config['model_providers'].items():
                definition['base_url'] = f'http://127.0.0.1:{server.server_port}/{p}'
                definition['request_max_retries'] = 0
            home = root / 'native'
            home.mkdir()
            (home / 'config.toml').write_text('\n'.join(k + '=' + launcher.inline_toml(v) for k, v in config.items()))
            refs = {}
            for p in launcher.KEY_ENV:
                credential = root / (p + '-auth.json')
                credential.write_text(json.dumps({'key': 'test-' + p}))
                refs[p] = {'path': str(credential), 'field': 'key'}
            native = os.environ['CC_CODEX_NATIVE_TEST_BIN']
            (root / 'launcher.json').write_text(json.dumps({'native_binary': native, 'openai_proxy': 'http://127.0.0.1:1', 'credential_files': refs}))
            environment = dict(os.environ, CODEX_HOME=str(home), CODEX_PROVIDER_CONFIG_DIR=str(root), NO_PROXY='127.0.0.1,localhost', no_proxy='127.0.0.1,localhost')
            for n in ('http_proxy', 'https_proxy', 'HTTP_PROXY', 'HTTPS_PROXY', 'all_proxy', 'ALL_PROXY'):
                environment.pop(n, None)
            script = Path(__file__).with_name('codex_launcher.py').resolve()
            for name in launcher.LAUNCHERS:
                (root / name).symlink_to(script)

            def run(command):
                result = subprocess.run(command, env=environment, cwd=root, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0, result.stderr[-2500:])
                events = [json.loads(line) for line in result.stdout.splitlines() if line.startswith('{')]
                self.assertTrue(any(e.get('type') == 'turn.completed' for e in events), result.stdout[-2000:])
                return next(e['thread_id'] for e in events if e['type'] == 'thread.started')

            def check_history(thread, provider):
                path = next((home / 'sessions').glob('**/*' + thread + '.jsonl'))
                events = [json.loads(line) for line in path.read_text().splitlines()]
                self.assertEqual(events[0]['payload']['model_provider'], provider)
                contexts = [e['payload'] for e in events if e['type'] == 'turn_context']
                self.assertTrue(contexts)
                self.assertTrue(all(e['sandbox_policy']['type'] == 'danger-full-access' for e in contexts))

            for public, provider in launcher.LAUNCHERS.items():
                with self.subTest(provider=provider):
                    command = [sys.executable, str(root / public)]
                    thread = run(command + ['exec', '--skip-git-repo-check', '--json', 'fixture'])
                    self.assertEqual(run(command + ['exec', 'resume', '--skip-git-repo-check', '--json', thread, 'fixture again']), thread)
                    check_history(thread, launcher.NATIVE_IDS[provider])
                    self.assertEqual(requests[-1][1], 'Bearer test-' + provider)
                    self.check_stdio(script, environment, root, None, provider, launcher.NATIVE_IDS[provider], config['model_providers'][launcher.NATIVE_IDS[provider]])

            for provider, aliases in launcher.ALIASES.items():
                for alias in aliases:
                    with self.subTest(legacy=alias):
                        definition = config['model_providers'][launcher.NATIVE_IDS[provider]]
                        env_name = launcher.KEY_ENV[provider]
                        environment[env_name] = 'test-' + provider
                        thread = run([native, '-c', 'model_providers=' + launcher.inline_toml({alias: definition}), '-c', 'model_provider=' + launcher.inline_toml(alias), 'exec', '--skip-git-repo-check', '--json', 'legacy fixture'])
                        public = next(n for n, p in launcher.LAUNCHERS.items() if p == provider)
                        self.assertEqual(run([sys.executable, str(root / public), 'exec', 'resume', '--skip-git-repo-check', '--json', thread, 'resume fixture']), thread)
                        check_history(thread, alias)
                        self.check_stdio(script, environment, root, thread, provider, alias, definition)

    def check_stdio(self, script, environment, root, thread, provider, alias, definition):
        process = subprocess.Popen([sys.executable, str(script), 'app-server', '-c', 'model_provider=' + launcher.inline_toml(provider), '-c', 'model_providers=' + launcher.inline_toml({provider: definition})], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, env=environment, cwd=root)
        replies = queue.Queue()
        def read():
            for line in process.stdout:
                replies.put(json.loads(line))
        threading.Thread(target=read, daemon=True).start()
        def rpc(identifier, method, params):
            process.stdin.write(json.dumps({'id': identifier, 'method': method, 'params': params}) + '\n')
            process.stdin.flush()
            for _ in range(100):
                reply = replies.get(timeout=15)
                if reply.get('id') == identifier:
                    self.assertNotIn('error', reply, str(reply))
                    return reply['result']
            self.fail('RPC reply missing')
        try:
            rpc(1, 'initialize', {'clientInfo': {'name': 'launcher-test', 'version': '1'}, 'capabilities': {'experimentalApi': True}})
            process.stdin.write(json.dumps({'method': 'initialized'}) + '\n')
            process.stdin.flush()
            params = {'modelProvider': provider, 'approvalPolicy': 'never', 'sandbox': 'danger-full-access'}
            if thread:
                params['threadId'] = thread
            result = rpc(2, 'thread/resume' if thread else 'thread/start', params)
            if thread:
                self.assertEqual(result['thread']['id'], thread)
            self.assertEqual(result['modelProvider'], alias)
        finally:
            process.stdin.close()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.terminate()
                process.wait(timeout=5)
            process.stdout.close()


if __name__ == "__main__":
    unittest.main()
