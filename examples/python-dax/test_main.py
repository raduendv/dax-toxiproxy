"""Offline failure-path tests; the separate --self-test uses a real Toxiproxy API."""
import unittest
from unittest.mock import Mock
from main import DaxSockets, Router


class RoutingTests(unittest.TestCase):
    def test_remote_api_refused(self):
        with self.assertRaises(ValueError):
            Router("http://192.0.2.1:8474")

    def test_mapping_failure_never_dials_upstream(self):
        router = Router("http://127.0.0.1:18474")
        router.request = Mock(side_effect=RuntimeError("API unavailable"))
        original = Mock()
        adapter = DaxSockets(router, original)
        with self.assertRaisesRegex(RuntimeError, "API unavailable"):
            adapter.create_connection(("node.example", 8111))
        original.create_connection.assert_not_called()
        with self.assertRaisesRegex(RuntimeError, "earlier routing failure"):
            router.mapping(("other.example", 8111))
        self.assertEqual(router.request.call_count, 1)


if __name__ == "__main__":
    unittest.main()