"""Read-only DAX fault example; run beside Toxiproxy on a DAX-reachable host.

Only amazondax.Tube's socket reference is adapted. The standard socket module,
credential-provider HTTP calls, and other application networking are untouched.
This is a version-pinned test adapter, not a supported SDK extension point.
See README.md before running against a test cluster.
"""

import argparse
import importlib
import importlib.metadata
import json
import os
import signal
import socket
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

from amazondax import AmazonDaxClient
from botocore.config import Config


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Do not let a local admin API redirect control requests to another host."""

    def redirect_request(self, request, response, code, message, headers, new_url):
        return None


class Router:
    """Own per-destination proxies, active fault state, and cleanup names.

    Discovery and connection pools use background threads. Hold one lock across
    each control operation to prevent duplicate mappings or partial phase changes
    being observed by new connections. A control failure is retained: continuing
    with uncertain fault coverage would produce misleading results.
    """

    def __init__(self, api):
        parsed = urllib.parse.urlparse(api)
        if (parsed.scheme != "http" or parsed.hostname != "127.0.0.1"
                or not parsed.port or parsed.path not in ("", "/")
                or parsed.username or parsed.password or parsed.query or parsed.fragment):
            raise ValueError("TOXIPROXY_API must be http://127.0.0.1:PORT")
        self.api = api.rstrip("/")
        self.prefix = f"python-dax-{os.getpid()}-{time.time_ns()}"
        self.routes = {}
        self.latency = 0
        self.failure = None
        self.closed = False
        self.lock = threading.RLock()
        self.http = urllib.request.build_opener(NoRedirect())

    def request(self, method, path, body=None):
        """HTTP is the control plane; DAX protocol bytes use separate TCP sockets."""
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.api + path, data=data, method=method,
                                         headers={"Content-Type": "application/json"})
        with self.http.open(request, timeout=5) as response:
            payload = response.read(1 << 20)
            return json.loads(payload) if payload else None

    def toxic(self, mapping, milliseconds):
        # Downstream means server-to-client bytes, not client TCP handshake delay.
        self.request("POST", f"/proxies/{mapping['name']}/toxics", {
            "name": "example_latency", "type": "latency", "stream": "downstream",
            "toxicity": 1, "attributes": {"latency": milliseconds, "jitter": 0},
        })

    def mapping(self, address):
        with self.lock:
            if self.closed:
                raise RuntimeError("router closed")
            if self.failure:
                raise RuntimeError("earlier routing failure") from self.failure
            try:
                host, port = address[:2]
                if port != 8111 or not host or ":" in host:
                    raise ValueError("Only plaintext IPv4 DAX on port 8111 is supported")
                upstream = f"{host}:8111"
                if upstream in self.routes:
                    return self.routes[upstream]
                wanted = {"name": f"{self.prefix}-{len(self.routes)}", "listen": "127.0.0.1:0",
                          "upstream": upstream, "enabled": True}
                # Track the name before POST in case the response is lost after creation.
                self.routes[upstream] = wanted
                created = self.request("POST", "/proxies", wanted)
                listen_host, listen_port = created["listen"].rsplit(":", 1)
                if (created["name"] != wanted["name"] or created["upstream"] != upstream
                        or not created["enabled"] or listen_host != "127.0.0.1"
                        or not 0 < int(listen_port) <= 65535):
                    raise ValueError("Invalid proxy mapping")
                self.routes[upstream] = created
                # Newly discovered nodes must inherit the fault before their first dial.
                if self.latency:
                    self.toxic(created, self.latency)
                print(f"route {created['name']}: {created['listen']} -> {upstream}", flush=True)
                return created
            except Exception as error:
                self.failure = error
                raise

    def set_latency(self, milliseconds):
        with self.lock:
            if self.failure:
                raise RuntimeError("earlier routing failure") from self.failure
            try:
                for mapping in self.routes.values():
                    if milliseconds:
                        self.toxic(mapping, milliseconds)
                    elif self.latency:
                        self.request("DELETE", f"/proxies/{mapping['name']}/toxics/example_latency")
                self.latency = milliseconds
            except Exception as error:
                self.failure = error
                raise

    def close(self):
        """Try every owned deletion; never reset a shared Toxiproxy server."""
        with self.lock:
            self.closed = True
            failures = []
            for mapping in self.routes.values():
                try:
                    self.request("DELETE", f"/proxies/{mapping['name']}")
                except Exception as error:
                    failures.append(str(error))
            if failures:
                raise RuntimeError("Proxy cleanup failed: " + "; ".join(failures))


class DaxSockets:
    """Socket-module facade used only inside the pinned SDK's Tube module.

    Delegate constants/socket options normally. Replace only create_connection,
    which the verified SocketTubePool._alloc implementation calls for every
    seed/node socket. Never fall back to the upstream when proxy setup fails.
    """

    def __init__(self, router, original):
        self.router = router
        self.original = original

    def __getattr__(self, name):
        return getattr(self.original, name)

    def create_connection(self, address, *args, **kwargs):
        mapping = self.router.mapping(address)
        host, port = mapping["listen"].rsplit(":", 1)
        return self.original.create_connection((host, int(port)), *args, **kwargs)


def positive_env(name, fallback):
    value = float(os.environ.get(name, fallback))
    if not 0 < value < float("inf"):
        raise ValueError(f"{name} must be finite and positive")
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", help="test routing against Toxiproxy without AWS")
    options = parser.parse_args()
    if importlib.metadata.version("amazon-dax-client") != "2.1.0":
        raise RuntimeError("Keep amazon-dax-client pinned to 2.1.0 for this internal adapter")
    router = Router(os.environ.get("TOXIPROXY_API", "http://127.0.0.1:18474"))
    tube = importlib.import_module("amazondax.Tube")
    original = tube.socket
    tube.socket = DaxSockets(router, original)
    stopping = threading.Event()
    old_handlers = {number: signal.signal(number, lambda *_: stopping.set())
                    for number in (signal.SIGINT, signal.SIGTERM)}
    try:
        router.request("GET", "/version")
        if options.self_test:
            # Test the exact socket facade referenced by the SDK, not AWS auth.
            with tube.socket.create_connection(("127.0.0.1", 8111), timeout=2):
                pass
            router.mapping(("127.0.0.1", 8111))
            assert len(router.routes) == 1
            router.set_latency(100)
            router.mapping(("127.0.0.2", 8111))
            router.set_latency(0)
            assert len(router.routes) == 2
            print("Python routing self-test passed")
            return
        endpoint = os.environ.get("DAX_ENDPOINT", "")
        table = os.environ.get("TABLE_NAME")
        if not endpoint.startswith("dax://") or not table:
            raise ValueError("Set DAX_ENDPOINT=dax://... and TABLE_NAME")
        key = json.loads(os.environ.get("DAX_KEY_JSON", '{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}'))
        duration = positive_env("DAX_DURATION_SECONDS", 10)
        timeout = positive_env("DAX_TIMEOUT_MS", 200) / 1000
        latency = positive_env("DAX_LATENCY_MS", 1000)
        if not latency.is_integer():
            raise ValueError("DAX_LATENCY_MS must be a whole number")
        # Read/connect timeouts are socket-stage bounds, not a universal hard
        # operation deadline. Credential loading and discovery can take longer.
        config = Config(connect_timeout=timeout, read_timeout=timeout, retries={"max_attempts": 0})
        with AmazonDaxClient(endpoint_url=endpoint, region_name=os.environ.get("AWS_REGION", "eu-west-1"), config=config) as client:
            for phase in ("healthy", "fault", "recovery"):
                if stopping.is_set():
                    raise InterruptedError("Interrupted")
                router.set_latency(int(latency) if phase == "fault" else 0)
                result = {"phase": phase, "successes": 0, "failures": 0}
                end = time.monotonic() + duration
                while time.monotonic() < end and not stopping.is_set():
                    try:
                        # An empty GetItem response is still success. No data is written.
                        client.get_item(TableName=table, Key=key)
                        result["successes"] += 1
                    except Exception as error:
                        result["failures"] += 1
                        result["last_error"] = str(error)
                    stopping.wait(0.1)
                print(json.dumps(result), flush=True)
                if stopping.is_set():
                    raise InterruptedError("Interrupted")
                if phase == "healthy" and (not result["successes"] or result["successes"] / (result["successes"] + result["failures"]) < 0.95):
                    raise RuntimeError("Healthy prerequisite failed; check credentials, key, connectivity, and timeout")
                if phase == "recovery" and not result["successes"]:
                    raise RuntimeError("No recovery progress")
    finally:
        # The client's context manager closes discovery/pools before we delete
        # listeners. Keep the facade installed until cleanup blocks future dials.
        try:
            router.close()
        finally:
            tube.socket = original
            for number, handler in old_handlers.items():
                signal.signal(number, handler)


if __name__ == "__main__":
    main()