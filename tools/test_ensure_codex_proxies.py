import tempfile
from pathlib import Path
import unittest
from unittest.mock import patch, Mock

import ensure_codex_proxies as relay


class RelayStartupTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.service = dict(name="aidp", url="http://127.0.0.1:19617",
                            listen="127.0.0.1:19617", session="codex-provider-aidp",
                            config="/config/aidp.toml",
                            log=str(Path(self.directory.name) / "relay.log"))

    def test_healthy_service_is_not_restarted(self):
        with patch.object(relay, "healthy", return_value=True), patch.object(relay.subprocess, "run") as run:
            relay.ensure_service("/bin/relay", self.service)
            run.assert_not_called()

    def test_check_does_not_start_missing_service(self):
        with patch.object(relay, "healthy", return_value=False), patch.object(relay.subprocess, "run") as run:
            with self.assertRaises(RuntimeError):
                relay.ensure_service("/bin/relay", self.service, True)
            run.assert_not_called()

    def test_start_clears_tmux_environment_and_tolerates_concurrent_starter(self):
        with patch.object(relay, "healthy", side_effect=[False, False, True]), \
                patch.object(relay.subprocess, "run", return_value=Mock(returncode=1)) as run, \
                patch.object(relay.time, "sleep"):
            relay.ensure_service("/bin/relay", self.service)
        self.assertEqual(run.call_count, 2)
        command = run.call_args.args[0]
        self.assertEqual(command[:3], ["tmux", "new-session", "-d"])
        self.assertIn("exec env -i HOME=", command[-1])
        self.assertIn("/bin/relay --config /config/aidp.toml --listen 127.0.0.1:19617", command[-1])
        self.assertEqual(Path(self.service["log"]).stat().st_mode & 0o777, 0o600)

    def test_unhealthy_existing_session_is_preserved(self):
        with patch.object(relay, "healthy", return_value=False), \
                patch.object(relay.subprocess, "run", return_value=Mock(returncode=0)) as run, \
                patch.object(relay.time, "sleep"):
            with self.assertRaises(RuntimeError):
                relay.ensure_service("/bin/relay", self.service)
        self.assertEqual(run.call_count, 1)


if __name__ == "__main__":
    unittest.main()
