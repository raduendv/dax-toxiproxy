# Java DAX Example

This Maven app uses `software.amazon.dax:amazon-dax-client` **2.0.11** (the DAX
SDK for AWS SDK for Java v2). It reads one existing key through per-node proxies,
injects downstream latency, and measures recovery without writing data.

First follow the [shared setup guide](../../docs/dax-examples.md) for the dedicated
server, cluster access, credentials, typed key, and interpretation rules.

## 1. Build And Test

Use a JDK 17+ and Maven. JDK 17 or 21 is a conservative runtime choice. The local
compile/self-test also passed with JDK 24, which emits upstream Netty Unsafe API
deprecation warnings. Missing SLF4J-binding warnings do not invalidate the test.

```sh
cd "$TOXIPROXY_HOME/examples/java-dax"
mvn -B test
```

The POM explicitly includes the AWS Netty transport at the DAX package's AWS SDK
version, because the adapter references Netty types at compile time. The toolkit
root has no Maven dependency. Unit tests need neither credentials nor a proxy.

## 2. Exercise The Adapter Without AWS

With the dedicated Toxiproxy server running:

```sh
mvn -B compile exec:java -Dexec.args=--self-test
```

This opens a real Netty connection through the registered handler, creates a new
destination while latency is active, removes the toxic, and deletes its mappings.
It does not perform DAX authentication or a GetItem request.

## 3. Run The Cluster Test

Set `DAX_ENDPOINT`, `TABLE_NAME`, `AWS_REGION`, and `DAX_KEY_JSON` from the shared
guide, then:

```sh
mvn -B compile exec:java
```

`DAX_DURATION_SECONDS` defaults to 10 per phase, `DAX_TIMEOUT_MS` to 200, and
`DAX_LATENCY_MS` to 1000. Set integer positive values. The typed key parser accepts
S/N attributes and the official SDK uses its normal credential provider chain.

## How The Code Works

Read [src/main/java/Main.java](src/main/java/Main.java) in this order:

1. `Router` manages HTTP control calls, owned proxy names, latency, and retained
   errors. Synchronized mapping/phase methods serialize discovery and fault changes.
2. `mapping` creates a listener for each original seed/node destination, validates
   the response, and applies any current toxic before returning a local address.
3. `ProxyEventLoops` extends Netty's event-loop group. The DAX SDK exposes the
   `Configuration.eventLoopGroup` setting publicly. On channel registration, the
   group adds an outbound handler that rewrites the `connect` destination only.
4. That handler performs HTTP mapping work on a separate future, not on the
   event-loop thread, then schedules the actual connect on the original executor.
   Errors fail the promise and close the channel; no direct-DAX fallback exists.
5. One `ClusterDaxClient` uses the real cluster URL, zero retries, and the custom
   group. It runs sequential `getItem` calls during healthy/fault/recovery phases.
6. The client closes first, the event loops shut down next, and the router deletes
   only its own proxies. A JVM shutdown hook allows up to 30 seconds for the main
   thread's cleanup after graceful interruption. SIGKILL cannot run cleanup.

## Limits And Troubleshooting

This adapter uses public Netty configuration rather than reflection, but still
depends on the pinned DAX client's Netty connection path. Self-test after changing
SDK/Netty versions. Only plaintext IPv4 DAX on port 8111 is covered; the example
does not alter hostname verification or implement TLS interception.

A phase's duration does not forcibly cancel credential loading or every SDK
background action. Individual operations use the configured SDK timeout; inspect
real elapsed time and failures. An empty item response is valid success.

Unit tests and a real-Toxiproxy self-test passed locally. No live AWS DAX run was
performed. The shared guide explains why a zero process exit is not proof that
all requests timed out during the fault or that recovery fully restored latency.