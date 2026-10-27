import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("codex_seed", Path(__file__).with_name("codex-seed.py"))
seed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(seed)


class SeedKeyTests(unittest.TestCase):
    def test_environment_precedes_private_file(self):
        self.assertEqual(seed.router_key(Path("/nonexistent"), {"CC_CODE_ROUTER_API_KEY": " test-key "}), "test-key")

    def test_private_file_and_malformed_value(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            (home / ".codex").mkdir()
            path = home / ".codex/code-router-auth.json"
            path.write_text('{"api_key":"test-only"}')
            self.assertEqual(seed.router_key(home, {}), "test-only")
            path.write_text('{"api_key":1234}')
            with self.assertRaises(ValueError) as result:
                seed.router_key(home, {})
            self.assertNotIn("1234", str(result.exception))


if __name__ == "__main__":
    unittest.main()
