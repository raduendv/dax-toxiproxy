# Python DAX Example

This app uses the official `amazon-dax-client` **2.1.0** to issue read-only
`get_item` calls through per-node Toxiproxy mappings. It runs healthy, latency
fault, and recovery phases against an existing test table.

Follow the [shared setup guide](../../docs/dax-examples.md) first: the client and
server need to share a DAX-reachable host, and the table key must match its schema.

## 1. Install In A Virtual Environment

Use Python 3.11+ with venv support. The example was checked locally on Python
3.15; no editor interpreter setting is changed by these commands.

```sh
cd "$TOXIPROXY_HOME/examples/python-dax"
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m unittest -v
.venv/bin/python main.py --help
```

The DAX SDK is pinned in `requirements.txt`; transitive dependency resolution is
not a fully locked environment. Record `pip freeze` when preserving a run for
comparison. Do not install these dependencies into the toolkit root or globally.

## 2. Exercise The Adapter Without AWS

Start the dedicated Toxiproxy server described in the shared guide, then:

```sh
.venv/bin/python main.py --self-test
```

No AWS credentials are needed. This invokes the socket facade referenced by the
DAX module, checks mapping reuse, adds a second destination during a fault, and
removes the toxic. Owned mappings are deleted before exit.

## 3. Run The Cluster Test

Export the shared guide's `DAX_ENDPOINT`, `TABLE_NAME`, `AWS_REGION`, and
`DAX_KEY_JSON`, then:

```sh
.venv/bin/python main.py
```

Use low-level key JSON such as `{"id":{"N":"42"}}`, not `{"id":42}`. This
walkthrough covers S/N keys. The normal botocore credential chain supplies AWS
credentials. `DAX_DURATION_SECONDS`, `DAX_TIMEOUT_MS`, and `DAX_LATENCY_MS` control
phase duration, socket timeout, and injected latency respectively.

## How The Code Works

Read [main.py](main.py) in this order:

1. `Router` owns the HTTP control client, mapping names, active latency, and a lock.
   Background discovery can race with requests; the lock makes mapping creation
   and phase transitions consistent.
2. `mapping` creates a `127.0.0.1:0` listener for each original node address, checks
   the returned mapping, and installs the current toxic before a new socket opens.
3. `DaxSockets` delegates ordinary socket attributes, but redirects
   `create_connection` through the mapping. Only `amazondax.Tube.socket` is
   replaced; the process-wide `socket` module and credential networking are not.
4. `main` checks the pinned SDK version, installs the adapter **before** client
   construction, and preserves the real seed endpoint for discovery.
5. A single `AmazonDaxClient` context manager runs the three read phases with
   retries disabled. Each phase prints a JSON result with counts and a last error.
6. Exiting that context closes the client before `Router.close` removes owned
   proxies. Finally, the original DAX socket reference and signal handlers return.

## Limits And Troubleshooting

`SocketTubePool._alloc` calling its module's `socket.create_connection` is an
**internal, version-specific** implementation detail. The facade is test-only;
keep the SDK pinned, run it in a dedicated process, and self-test after upgrades.
TLS and IPv6 destinations are deliberately outside this example's scope.

`DAX_TIMEOUT_MS` sets botocore read/connect timeouts used by DAX sockets; it is not
a strict wall-clock deadline for an entire operation, credential lookup, or
discovery. SIGINT/SIGTERM mark the app for shutdown; a current blocking call may
finish first. A `finally` block attempts all proxy deletions, even after errors.

Unit tests exercise fail-closed behavior without AWS or Toxiproxy. The self-test
uses Toxiproxy but no DAX protocol. Real cluster authentication and fault/recovery
outcomes remain unverified locally; follow the shared guide to inspect them.