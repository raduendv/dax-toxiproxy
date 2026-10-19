# .NET DAX Example

This .NET 8 app uses the official AWS
[AWSSDK.DAX.Client 4.1.0](https://www.nuget.org/packages/AWSSDK.DAX.Client/4.1.0),
not the DAX management API or an ordinary DynamoDB HTTPS client. It performs
read-only `GetItemAsync` requests through Toxiproxy and measures three phases.

**Important:** this SDK has no public dial hook. The example uses a test-only,
in-process Harmony patch of a specific internal SDK method. Keep it in a dedicated
test process. It is not a supported AWS extension point or production adapter.

Follow the [shared setup guide](../../docs/dax-examples.md) first for server
startup, VPC access, IAM, table keys, counts, and cleanup.

## 1. Build

Install the .NET 8 SDK or a newer SDK capable of targeting/running .NET 8:

```sh
cd "$TOXIPROXY_HOME/examples/dotnet-dax"
dotnet restore
dotnet build --no-restore -warnaserror
```

The project pins the DAX SDK to 4.1.0 and `Lib.Harmony` to 2.4.3. A normal JIT
runtime is required; **NativeAOT, trimming, and unsupported runtime-patching
platforms are not supported**. The adapter's self-test passed locally on macOS
ARM64 with .NET 8.0.406. Validate on the actual test host before using DAX.

## 2. Exercise The Actual Patched Method Without AWS

With the dedicated Toxiproxy server from the shared guide running:

```sh
dotnet run --no-build -- --self-test
```

This installs the Harmony patch, invokes the SDK's internal TCP-connect method,
adds another destination during latency, removes the toxic, and deletes owned
proxies. It needs no AWS credentials. If patch installation fails, stop: never
remove the adapter and interpret an unproxied DAX run as a fault test.

The self-test also parses a numeric key. It is a routing/control integration
check, not a test of DAX authentication or GetItem protocol behavior.

## 3. Run The Cluster Test

Export `DAX_ENDPOINT`, `TABLE_NAME`, `AWS_REGION`, and `DAX_KEY_JSON` as shown in
the shared guide, then run:

```sh
dotnet run --no-build
```

Optional positive integer settings are `DAX_DURATION_SECONDS` (default 10 per
phase), `DAX_TIMEOUT_MS` (200), and `DAX_LATENCY_MS` (1000). Key JSON accepts S/N
types, for example `{"id":{"N":"42"}}`. AWS's default credential identity
resolver supplies credentials; do not embed keys in the source.

## How The Code Works

Read [Program.cs](Program.cs) in this order:

1. `Router` owns a loopback-only HTTP client and the names of this run's mappings.
   `SemaphoreSlim` serializes SDK background discovery with phase transitions.
2. `Mapping` uses the original `HostPort` as the upstream and creates a loopback
   listener on a dynamically allocated port. It validates the response and adds
   the active toxic before returning a listener to a new connection.
3. `RoutingPatch.Target` resolves the pinned SDK's
   `Amazon.DAX.Internal.TcpClientTubeFactory.ConnectTcpClientAsync` method by its
   exact parameter signature. An absent signature fails startup.
4. The Harmony prefix skips the original connect and supplies a `Task` that
   connects the SDK-owned `TcpClient` to the proxy listener. All subsequent DAX
   framing, signing, and authentication remain the official SDK's responsibility.
   Other `TcpClient` calls and credential-provider HTTP traffic are not patched.
5. `Run` creates the DAX client with the original discovery endpoint, zero retries,
   and explicit timeouts. Each sequential read has a cancellation token; phase
   summaries report actual successes/failures, not assumed timeout outcomes.
6. Ctrl+C and SIGTERM cancel the run. Cleanup disposes the DAX client, deletes owned
   mappings with independent HTTP timeouts, then removes this run's patch. The
   original error is not replaced by an unpatch attempt when installation failed.

## Limits And Troubleshooting

Runtime patching is deliberately isolated and version-specific. Do not upgrade
the DAX or Harmony package without rerunning the self-test. Do not host this app
inside another process with unrelated DAX clients: the SDK method patch applies
process-wide even though its target is narrow. The patch preserves the original
destination for proxy creation but changes the underlying TCP-connect behavior.

Only plaintext IPv4 DAX on port 8111 is supported. Initialization, credential
resolution, and cleanup may exceed one request timeout. Forced process exit skips
cleanup; stop the dedicated server to close abandoned listeners. No live AWS DAX
run was performed locally, so validate real requests and recovery in your VPC.