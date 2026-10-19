# Go DAX Example

This standalone Go app uses the official `aws-dax-go-v2` **v1.0.3** client to
read an existing table key through Toxiproxy. It establishes a healthy baseline,
injects downstream latency, then checks whether the same client makes progress
after the toxic is removed. It never creates tables or writes items.

Start with the [step-by-step Go walkthrough](../../docs/go-dax-test.md) for
server startup and evidence inspection. The [shared setup guide](../../docs/dax-examples.md)
explains network access, credentials, and differences between the language examples.

## 1. Install And Check

Use Go **1.26+**. This directory has its own module and dependency checksums;
no separate client project, SDK checkout, or local module replacement is needed.
Set `TOXIPROXY_HOME` to the absolute path of this toolkit checkout:

```sh
export TOXIPROXY_HOME=/absolute/path/to/dax-toxiproxy
cd "$TOXIPROXY_HOME/examples/go-dax"
GOWORK=off go mod download
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
GOWORK=off go run -mod=readonly . -h
```

`GOWORK=off` prevents an enclosing Go workspace from changing dependency selection.
The tests use a local fake control API and TCP listener. They need neither AWS
credentials nor a running Toxiproxy server. Unlike the other examples, this app
has **no `--self-test` mode**; use its Go tests for offline routing checks.

## 2. Start The Server And Prepare AWS Access

Follow the walkthrough to start a dedicated Toxiproxy server on the same host
as this app, with its API bound to loopback. That host must already reach the
cluster seed and every discovered node on TCP port 8111, normally from an EC2
instance in the VPC. Proxying only the seed would miss discovered-node traffic.

Configure AWS credentials through the SDK's normal provider chain, such as a
profile or instance role, with DAX read access to the test table. Use a plaintext
`dax://` test cluster. TLS (`daxs://`) and IPv6 destinations are outside this
example's scope; do not disable production encryption to run it.

## 3. Run The Cluster Test

Replace these values with your test resources. Keep the server-setup shell's
environment so you can stop its owned process afterward:

```sh
export DAX_ENDPOINT=dax://your-cluster.example.dax-clusters.eu-west-1.amazonaws.com
export AWS_REGION=eu-west-1
export TABLE_NAME=your_test_table
export DAX_KEY_JSON='{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}'
export TOXIPROXY_API=http://127.0.0.1:18474

cd "$TOXIPROXY_HOME/examples/go-dax"
GOWORK=off go run -mod=readonly . \
  -proxy-api "$TOXIPROXY_API" \
  -endpoint "$DAX_ENDPOINT" -region "$AWS_REGION" -table "$TABLE_NAME" \
  -key "$DAX_KEY_JSON" \
  -duration 10s -request-timeout 200ms -interval 100ms -latency 1s
```

The app reads **CLI flags**, not the other examples' `DAX_DURATION_SECONDS`,
`DAX_TIMEOUT_MS`, or `DAX_LATENCY_MS` environment settings. The environment
variables above are expanded by the shell into those flags.

Match the full key to the table's actual primary-key schema. Use typed DynamoDB
JSON, not document-style JSON: a numeric partition key without a sort key could
be `-key '{"id":{"N":"42"}}'`. String, number, and binary key types are supported;
binary values use base64 strings. Missing items still produce successful reads.

`-duration` applies to **each** phase. Requests are sequential and paced by the
ticker; slow responses reduce the offered rate. Retries are disabled. Allow more
than 30 seconds for the default three-phase run, including startup and cleanup.

## How The Code Works

Read [main.go](main.go) in this order:

1. `proxy` describes a local listener and its original DAX upstream. `router`
   owns the mapping names, active fault, mutex, and retained control errors.
2. `request` sends bounded HTTP control calls. This is separate from the DAX
   protocol traffic, which the SDK sends over TCP through the listeners.
3. `mapping` creates one proxy per original seed/node address, requesting
   `127.0.0.1:0` so Toxiproxy allocates a free port. It validates the response and
   installs the active toxic before allowing a newly discovered node to connect.
4. `dial` is installed through the SDK's **public `DialContext` hook**. It keeps
   the real cluster URL for discovery but connects sockets to validated listeners.
   A mapping error fails the dial; there is no direct-DAX fallback or runtime patch.
5. `run` loads credentials, creates one client with retries disabled, and reuses
   it during healthy, fault, and recovery phases. Each GetItem has its own context
   deadline bounded by the phase deadline. Phase-end cancellations are not counted.
6. Deferred cleanup closes the SDK before deleting this run's mappings. Cleanup
   uses independent contexts so canceling the experiment does not cancel deletion.
   `main` reports errors only after those defers have run.

## Results And Cleanup

The app prints plain-text `route ...` messages and one JSON result per phase:
`phase`, `successes`, `failures`, and an optional `last_error`. Failures include
all GetItem errors, not only timeouts. It requires at least 95% healthy success
and at least one recovery success, but does not assert a fault failure rate.

A zero exit code does not prove every fault-phase request timed out, that
recovery restored baseline latency, or that the SDK allocation bug was reproduced.
The toxic delays response bytes **after TCP acceptance**, not the client handshake.
Credential resolution, initialization, and cleanup do not share one universal
request deadline. The walkthrough shows how to save/filter logs and inspect metrics.

Ctrl+C and SIGTERM request cancellation and deferred cleanup; SIGKILL cannot.
The app deletes only its own proxies and leaves the server running. In the
original server-setup shell, confirm cleanup and stop the dedicated server:

```sh
cd "$TOXIPROXY_HOME"
scripts/toxiproxyctl.sh list
scripts/server.sh stop
```

Never use global `reset` on a shared instance. Preserve logs when diagnosing
unexpected results. The offline tests do not authenticate against DAX; validate
actual cluster reads and fault/recovery outcomes in your own test environment.