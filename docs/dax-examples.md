# DAX Examples: Shared Setup

The optional examples run real, read-only `GetItem` calls using the official DAX
client for each language. Each app creates its own per-node Toxiproxy mappings,
measures a healthy baseline, injects downstream latency, then tests recovery.
The server toolkit itself does not import or install any of their dependencies.

| Language | Guide | Client version | Routing strategy |
| --- | --- | --- | --- |
| Go | [Walkthrough](go-dax-test.md) | Go DAX v1.0.3 | Public `DialContext` hook |
| .NET | [Example](../examples/dotnet-dax/README.md) | AWSSDK.DAX.Client 4.1.0 | Test-only Harmony patch of SDK TCP connector |
| JavaScript | [Example](../examples/js-dax/README.md) | amazon-dax-client 1.2.10 | Test-only SDK `Connector` adapter |
| Python | [Example](../examples/python-dax/README.md) | amazon-dax-client 2.1.0 | Test-only DAX-module socket facade |
| Java | [Example](../examples/java-dax/README.md) | amazon-dax-client 2.0.11 | Public Netty event-loop configuration |

## 1. Choose A Safe Test Environment

Run the client and Toxiproxy on the **same host**, with existing network access to
the cluster, usually an EC2 test instance. `127.0.0.1` means that host, not your
laptop. The server must reach the cluster seed **and every discovered node** on
TCP port 8111. These examples do not modify security groups, routes, or the VPC.

Use an existing plaintext `dax://` test cluster and table. These examples do not
support `daxs://`, TLS interception, or IPv6 destinations. Do not disable encryption
on a production cluster to run them. Other clients connecting directly to DAX
are not affected by toxics on these app-owned proxy connections.

Configure normal AWS credentials using a profile, environment, or instance role
supported by the chosen SDK. The caller needs DAX data-plane read access to the
test table. Never put credentials in source code or commit them to the repository.

The table may have any key names with string or number types. All examples accept
low-level DynamoDB key JSON, not document-style values. Include the complete
primary key (partition key and sort key, if present). The Go app additionally
accepts binary keys. Missing items are fine: a successful empty response still
tests connectivity. No example creates tables, writes items, or deletes data.

## 2. Start A Dedicated Proxy Server

Use one shell for setup and the chosen app so these exported variables persist.
Set an **absolute** checkout path:

```sh
export TOXIPROXY_HOME=/absolute/path/to/dax-toxiproxy
cd "$TOXIPROXY_HOME"
export TOXIPROXY_API_HOST=127.0.0.1
export TOXIPROXY_API_PORT=18474
export TOXIPROXY_API=http://127.0.0.1:18474
export TOXIPROXY_RUN_DIR
TOXIPROXY_RUN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/dax-example.XXXXXX")
export TOXIPROXY_BINARY="$TOXIPROXY_HOME/bin/toxiproxy-server"
make build-server
scripts/server.sh start
scripts/server.sh status
scripts/toxiproxyctl.sh list
```

The version should be `2.12.0`, and the dedicated server should initially have no
proxies. If the API port is occupied, change **both** `TOXIPROXY_API_PORT` and
`TOXIPROXY_API`. Do not reuse an unknown server or run global `reset` against it.
All non-Go apps intentionally accept only an IPv4 loopback control API.

## 3. Verify Routing Without AWS

Install the chosen example's dependencies using its README, then run its
`--self-test`. This needs the local server but **no AWS credentials or cluster**.
It exercises the installed adapter, listener allocation, a second destination
created during the fault, toxic removal, and owned-proxy deletion.

Self-tests use loopback dummy upstreams on port 8111. Their client-to-proxy TCP
connect may succeed even when no upstream service is listening; they intentionally
do not send DAX protocol requests. They prove adapter/control wiring, **not** DAX
authentication, protocol compatibility under load, or fault/recovery behavior.

After the process exits, `scripts/toxiproxyctl.sh list` should again be empty.

## 4. Configure A Real DAX Read Test

Replace the endpoint, region, table, and key with your test resources:

```sh
export DAX_ENDPOINT=dax://your-cluster.example.dax-clusters.eu-west-1.amazonaws.com
export AWS_REGION=eu-west-1
export TABLE_NAME=your_test_table
export DAX_KEY_JSON='{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}'
export DAX_DURATION_SECONDS=10
export DAX_TIMEOUT_MS=200
export DAX_LATENCY_MS=1000
```

For a numeric partition key called `id` without a sort key:

```sh
export DAX_KEY_JSON='{"id":{"N":"42"}}'
```

The four non-Go apps use these environment variables. Go uses CLI flags instead;
its walkthrough shows the equivalent options. Duration, timeout, and latency
must be positive; use whole numbers for portability across these examples.

## 5. Run And Interpret The Phases

Run the chosen app as documented in its README. Defaults offer no more than about
10 sequential reads per second, and slower calls lower that rate. The same client
and pools are reused across all phases. New nodes inherit the current fault
before connecting. Routing/control errors never trigger a direct-DAX fallback.

1. **Healthy:** no toxic; require at least one success and at least 95% success.
   If it fails, fix credentials, schema, connectivity, or an unrealistic timeout
   before interpreting fault behavior.
2. **Fault:** add `example_latency` with downstream latency on every owned proxy.
   Downstream means server-to-client bytes after TCP acceptance. A 1000 ms toxic
   with a 200 ms SDK timeout is expected to increase errors, but the apps do not
   assert that every call fails. Buffering, pools, and SDK behavior affect results.
3. **Recovery:** remove only that named toxic and require at least one successful
   request. A success proves progress, not a return to the original error rate.

SDK retries are configured to zero. Timeout semantics differ: Python uses socket
read/connect bounds, while other clients also expose request timeouts. Credential
resolution, discovery, and cleanup are not universal hard-deadline operations.
Allow more than three times `DAX_DURATION_SECONDS` for a complete run.

The apps print `route ...` lines and a JSON object for each measured phase with
`phase`, `successes`, `failures`, and a last error when available. The error count
includes all request failures, not just timeouts. An empty item response counts
as success. A zero exit code alone does not prove the toxic caused timeouts or
demonstrate the Go SDK connection-allocation bug.

To retain and inspect output without hiding the app's exit status:

```sh
set -o pipefail
# Append this to the chosen language's run command: 2>&1 | tee /tmp/dax-example.log
jq -R 'fromjson? | select(.phase)' /tmp/dax-example.log
```

Inspect mapping targets and proxy metrics while the app is running:

```sh
"$TOXIPROXY_HOME/scripts/toxiproxyctl.sh" list
curl --fail --silent --show-error "$TOXIPROXY_API/metrics"
```

## 6. Clean Up

Normal completion, handled errors, and graceful interruption attempt to close
the SDK before deleting app-owned mappings. The apps do not stop the server.
After inspecting output, stop the dedicated server from the original shell:

```sh
cd "$TOXIPROXY_HOME"
scripts/toxiproxyctl.sh list
scripts/server.sh stop
```

Retain the server log in `TOXIPROXY_RUN_DIR` for diagnosis. Forced termination
(especially SIGKILL) skips app cleanup; stop this dedicated server to close its
remaining listeners. Shutdown may take time while in-flight operations finish.

## Compatibility And Verification

JavaScript, Python, and .NET depend on SDK internals for this demonstration.
Keep the SDK and adapter versions pinned, run self-tests after changes, and do
not embed them in a production or shared application process. The .NET patch
requires a supported JIT runtime and cannot be used with NativeAOT/trimming.

Offline unit tests, compilation, and real-Toxiproxy adapter self-tests have been
run locally. No live DAX/AWS integration run has been performed for these four
new examples. Verify all cluster/node mappings and actual phase outcomes in your
own test environment before relying on the results.