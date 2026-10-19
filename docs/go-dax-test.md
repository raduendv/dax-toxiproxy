# Go DAX Test Through Toxiproxy

This walkthrough runs the bundled [Go example app](../examples/go-dax/main.go)
against this toolkit's Toxiproxy server. It creates loopback proxy mappings
automatically and exercises healthy/fault/recovery phases. The app has its own
Go module in `examples/go-dax`; the server toolkit itself has no SDK dependency.

## 1. Prepare The Test Host

Run both Toxiproxy and the Go client on the same host, normally an EC2 instance
that already has network access to the DAX cluster. Localhost below means that
host, not your laptop. Toxiproxy must be able to reach every discovered DAX node
on TCP port 8111; access to only the cluster seed is insufficient.

You need:

- A checkout of `dax-toxiproxy`; no other project is required.
- Bash, curl, jq, and Go 1.26+ for the example app.
- AWS credentials available through the standard Go SDK credential chain, such
  as an EC2 instance role, with permission to read the test table through DAX.
- An existing test table and a valid key for its schema. The default key uses
  string attributes `pk` and `sk`; use `-key` for other names or S/N/B key types.
  Missing items are allowed: an empty successful GetItem response still counts
  as success. The app does not write or populate the table.
- A plaintext `dax://` cluster endpoint. This harness does not support TLS
  (`daxs://`); do not disable encryption on an existing cluster for this example.

Use a test cluster and table. The latency fault affects this client's proxied
connections, not clients that connect directly to DAX.

Set paths and replace the example cluster/table values:

```sh
export TOXIPROXY_HOME=/path/to/dax-toxiproxy
export AWS_REGION=eu-west-1
export DAX_ENDPOINT=dax://your-cluster.example.dax-clusters.eu-west-1.amazonaws.com
export TABLE_NAME=your_test_table
```

The app uses the published Go DAX SDK v1.0.3. It does not require a patched SDK,
local replacement paths, or the separate connection-allocation test project.

## 2. Start An Isolated Toxiproxy Server

Use a dedicated API port and run directory. Keep this shell open so subsequent
commands retain these environment variables.

```sh
cd "$TOXIPROXY_HOME"
export TOXIPROXY_API_HOST=127.0.0.1
export TOXIPROXY_API_PORT=18474
export TOXIPROXY_API=http://127.0.0.1:18474
export TOXIPROXY_RUN_DIR
TOXIPROXY_RUN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/go-dax-toxiproxy.XXXXXX")
export TOXIPROXY_BINARY="$TOXIPROXY_HOME/bin/toxiproxy-server"

make build-server
scripts/server.sh start
scripts/server.sh status
scripts/toxiproxyctl.sh list
```

The version should be `2.12.0` and the proxy list should initially be empty.
If startup refuses an occupied API port, choose another port and update both
`TOXIPROXY_API_PORT` and `TOXIPROXY_API`; do not reuse someone else's server.

## 3. Understand The Go Connection Hook

The example configures the Go DAX SDK as follows (excerpt; `routing` is the
app's concurrency-safe proxy router):

```go
cfg := dax.NewConfig(awsConfig, endpoint)
cfg.RequestTimeout = requestTimeout
cfg.ReadRetries = 0
cfg.WriteRetries = 0
cfg.DialContext = routing.dial

client, err := dax.New(cfg)
if err != nil {
    return err
}
defer client.Close()
```

The important part of the router's dial method is:

```go
mapping, err := route.mapping(address)
if err != nil {
    return nil, err
}
dialer := net.Dialer{Timeout: 5 * time.Second}
connection, err := dialer.DialContext(ctx, network, mapping.Listen)
```

`route.mapping(address)` creates or reuses a Toxiproxy proxy through
`POST /proxies`, with the SDK's original destination as `upstream`, a unique
name, and `127.0.0.1:0` as `listen`. It uses the actual listener port returned by
Toxiproxy. Mapping creation or validation failure returns an error, never a
direct connection to DAX. New mappings inherit the currently active fault.

The same dial hook handles the seed endpoint and subsequently discovered node
addresses. Keep the original DAX endpoint in `dax.NewConfig`: simply replacing
the seed endpoint with localhost would not intercept discovered nodes.

```text
Go DAX SDK --DialContext--> loopback proxy listener --> DAX seed/node:8111
     |
     +-- proxy router --HTTP--> 127.0.0.1:18474 (control API)
```

Request deadlines are passed separately to `client.GetItem(ctx, input)`; a
successful TCP connection to Toxiproxy is not proof that a DAX request succeeded.

## 4. Run Healthy, Fault, And Recovery Phases

This command performs sequential GetItem requests and logs route mappings plus
one JSON result per phase. Keep the same environment from step 2:

```sh
cd "$TOXIPROXY_HOME/examples/go-dax"
export OUTPUT
OUTPUT=$(mktemp "${TMPDIR:-/tmp}/go-dax-results.XXXXXX")
set -o pipefail

GOWORK=off go run -mod=readonly . \
  -proxy-api "$TOXIPROXY_API" \
  -endpoint "$DAX_ENDPOINT" -region "$AWS_REGION" -table "$TABLE_NAME" \
  -key '{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}' \
  -interval 100ms -duration 10s -request-timeout 200ms \
  -latency 1s | tee "$OUTPUT"
```

Replace the JSON with your table's actual key names and types. For a numeric
partition key named `id` with no sort key, use `-key '{"id":{"N":"42"}}'`.
Binary key values use base64 strings. This app repeatedly reads the same key;
it is a small connection/fault example, not a cache hit-rate benchmark.

The phases run automatically:

1. **Healthy:** no toxics. The client requires at least 95% successful requests
   before it proceeds. Fix connectivity, credentials, schema, or an overly short
   request deadline if this prerequisite fails.
2. **Fault:** add 1 second of downstream latency to every owned mapping, including
   mappings created during the phase. Expect more request failures/timeouts with
   a 200 ms deadline, but do not assume every call must fail: caching, pooling,
   buffering, and SDK behavior can affect the outcome.
3. **Recovery:** remove the injected toxic and continue requests. Inspect actual
   successes and latency to determine whether the client recovers.

`-duration` is per phase, so allow roughly 30 seconds plus initialization and
cleanup. Retries are disabled to make outcomes easier to interpret. Requests
are sequential: `-interval 100ms` offers at most 10 requests per second, and
slower responses reduce that rate. Press Ctrl+C for graceful cleanup.

While the command runs, another shell can inspect its node mappings:

```sh
TOXIPROXY_API=http://127.0.0.1:18474 \
  /path/to/dax-toxiproxy/scripts/toxiproxyctl.sh list
```

## 5. Inspect Evidence

```sh
jq -R 'fromjson? | select(.phase)' "$OUTPUT"
grep '^route ' "$OUTPUT"
```

Check that route lines point to real DAX destinations through loopback listeners.
Each phase result reports `phase`, `successes`, `failures`, and, on failure,
`last_error`. The app requires at least 95% healthy success and at least one
recovery success; it exits nonzero on routing/control/cleanup failures. It does
not require a specific fault failure rate. Inspect errors and counts rather
than treating a zero exit status as proof of the expected fault response.

The app logs only mapping creation, not every dial. For additional traffic
evidence, inspect Toxiproxy's metrics while it is running:

```sh
curl --fail --silent --show-error "$TOXIPROXY_API/metrics"
```

This tests response-path latency after TCP acceptance. It does not simulate a
slow client TCP handshake or by itself prove the connection-allocation bug.

## 6. Clean Up

On normal completion, Ctrl+C, SIGTERM, or a handled error, the app closes its DAX client
and deletes only its own proxy mappings. Confirm cleanup, then stop the dedicated
server from the original shell:

```sh
cd "$TOXIPROXY_HOME"
scripts/toxiproxyctl.sh list
scripts/server.sh stop
```

Keep the run directory's server log and the client's output file for
diagnosis. Forced termination such as SIGKILL can leave mappings behind; stopping
this dedicated server closes their listeners. Do not run global `reset` against
a shared server.

## Offline Checks

The example's tests use a local fake control API and TCP listener; no AWS
credentials or DAX cluster are required:

```sh
cd "$TOXIPROXY_HOME/examples/go-dax"
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go run . -h
```