# Toxiproxy Toolkit

A language-neutral toolkit for running the official [Shopify Toxiproxy](https://github.com/Shopify/toxiproxy) server and controlling its HTTP API. The toolkit root has **no Go module or language-specific client dependency**. Optional, independently runnable [DAX examples](docs/dax-examples.md) keep their own dependencies under `examples/`; none are needed to use the server tools.

Any client library can use it through the documented HTTP API or its language-specific Toxiproxy client. The examples use curl and JSON.

## DAX Client Examples

Start with the [shared setup guide](docs/dax-examples.md), then choose a language:

- [Go walkthrough](docs/go-dax-test.md): public SDK dial hook.
- [.NET example](examples/dotnet-dax/README.md): AWSSDK.DAX.Client with a test-only runtime patch.
- [JavaScript example](examples/js-dax/README.md): pinned SDK connector adapter.
- [Python example](examples/python-dax/README.md): scoped DAX socket adapter.
- [Java example](examples/java-dax/README.md): public Netty event-loop configuration.

Each guide covers installation, node discovery, healthy/fault/recovery testing,
results, and cleanup. The four non-Go apps include AWS-free routing self-tests
against a local server. Internal adapters are version-pinned and test-only.

## Requirements

- Bash 3.2+ and curl; jq is needed by `toxiproxyctl.sh` for JSON validation/building.
- Go 1.23+ only when building the pinned official server with `scripts/build-server.sh`.
- Go 1.26+ when running the optional Go DAX example.
- Alternatively, install the official `toxiproxy-server` binary or use the official container image outside this toolkit.

## Quick Start

```sh
make build-server
make start
make status
make list
make stop
```

The server API binds to `127.0.0.1:8474` by default. Proxy listeners should also bind to loopback unless clients on another host explicitly need to reach them. Do not expose the admin API publicly.

For an isolated instance and its own PID/log files:

```sh
TOXIPROXY_API_HOST=127.0.0.1 \
TOXIPROXY_API_PORT=18474 \
TOXIPROXY_RUN_DIR="$PWD/.toxiproxy-run" \
TOXIPROXY_BINARY="$PWD/bin/toxiproxy-server" \
scripts/server.sh start
```

Stop only the process recorded in that run directory:

```sh
TOXIPROXY_API_PORT=18474 \
TOXIPROXY_RUN_DIR="$PWD/.toxiproxy-run" \
scripts/server.sh stop
```

## Language-Neutral HTTP API

Set the API URL if needed:

```sh
export TOXIPROXY_API=http://127.0.0.1:8474
```

Create and inspect a proxy, add/remove a fault, then delete the proxy:

```sh
scripts/toxiproxyctl.sh create cache_test 127.0.0.1:16379 127.0.0.1:6379
scripts/toxiproxyctl.sh get cache_test
scripts/toxiproxyctl.sh toxic-add cache_test latency downstream '{"latency":100,"jitter":0}'
scripts/toxiproxyctl.sh toxic-list cache_test
scripts/toxiproxyctl.sh toxic-remove cache_test latency_downstream
scripts/toxiproxyctl.sh delete cache_test
```

The tool supports `list`, `create`, `get`, `enable`, `disable`, `delete`, `reset`, `toxic-add`, `toxic-list`, and `toxic-remove`. `reset` affects every proxy on that server, enabling all proxies and removing all toxics; use it only on a dedicated instance. See [examples/proxies.json](examples/proxies.json) for the official `/populate` format.

## Official Server Binary

The bootstrap downloads and builds **official Shopify Toxiproxy v2.12.0** without adding `go.mod` or `go.sum` to this project:

```sh
scripts/build-server.sh --output /tmp/toxiproxy-server
```

The default release is pinned in `scripts/build-server.sh`; override `TOXIPROXY_VERSION` to deliberately select another version. This script builds Toxiproxy itself only. It does not build or test a DAX client.

## Client Integration

A TCP proxy only affects traffic explicitly routed through it. DAX discovers backend node endpoints, so a DAX-specific integration must create a mapping for every discovered node and provide its client with a dialer that uses those mappings. That integration belongs in the client project; this repository remains reusable by Go, .NET, JavaScript, Python, and other clients.

See [client integration notes](docs/client-integration.md) for endpoint routing, isolation, and cleanup guidance.

For a runnable Go example included in this repository, follow the
[step-by-step Go DAX test](docs/go-dax-test.md).

Toxiproxy's latency toxic delays bytes on an established proxied connection. It does **not** simulate a slow client TCP handshake because Toxiproxy accepts that connection before forwarding upstream.

## Resource Safety

`scripts/server.sh` defaults the control API to loopback, refuses to reuse an existing API listener, and records a PID in a per-run directory. `stop` only signals the PID it started after verifying its process command. Configure `TOXIPROXY_RUN_DIR` per experiment to isolate PID and log files. `scripts/toxiproxyctl.sh` also rejects non-loopback control URLs unless `TOXIPROXY_ALLOW_REMOTE_API=1` is explicitly set.

The JSON examples and shell commands are protocol-level tooling. The optional
client apps demonstrate DAX node routing and healthy/fault/recovery testing, not
production benchmarks. Their dependencies are isolated from the toolkit root.
