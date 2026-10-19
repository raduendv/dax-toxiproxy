# JavaScript DAX Example

This Node.js app reads one existing table key through Toxiproxy, injects latency,
and measures recovery. It uses the official `amazon-dax-client` **1.2.10**. The
app, SDK, and proxy server must run on the same DAX-reachable host.

Start with the [shared setup guide](../../docs/dax-examples.md) for server startup,
IAM/network prerequisites, key JSON, result interpretation, and cleanup.

## 1. Install And Check

Use Node.js 20+ and npm. From the checkout:

```sh
cd "$TOXIPROXY_HOME/examples/js-dax"
npm ci --ignore-scripts
npm test
```

`package-lock.json` locks transitive dependencies. The DAX client currently
depends on AWS SDK for JavaScript v2, which is end-of-support. npm's deprecation
warnings are expected; this is a pinned test example, not a recommendation for
a new production dependency. Do not substitute the ordinary DynamoDB v3 HTTP
client: it does not speak the DAX data-plane protocol.

## 2. Exercise The Adapter Without AWS

With the dedicated server from the shared guide running:

```sh
npm start -- --self-test
```

This checks the actual SDK connector, two mappings, fault inheritance, and toxic
removal. It sends no DAX requests. Verify the proxy list is empty after it exits.

## 3. Run The Cluster Test

Set `DAX_ENDPOINT`, `TABLE_NAME`, `AWS_REGION`, and `DAX_KEY_JSON` as shown in the
shared guide, then run:

```sh
npm start
```

`DAX_DURATION_SECONDS` (default 10), `DAX_TIMEOUT_MS` (200), and `DAX_LATENCY_MS`
(1000) customize the phases. Use a complete typed S/N primary key. The app loads
credentials through the SDK's normal provider chain; an EC2 instance role is
convenient. It only calls `getItem`, never creates or populates the table.

## How The Code Works

Read [main.js](main.js) in this order:

1. `Router.request` talks to the loopback HTTP API with redirects refused.
2. `Router.exclusive` serializes asynchronous mapping and phase changes. A failed
   control call is retained so later dials cannot silently bypass the experiment.
3. `Router.mapping` creates one listener per original seed/node address. Port zero
   lets Toxiproxy choose a free port; the returned listener is validated.
4. `installRouting` replaces only the pinned SDK's `Connector.prototype.connect`.
   The SDK expects a socket immediately, so the adapter returns an unconnected
   `net.Socket`, then connects it after mapping/toxic setup finishes. Errors
   destroy that socket instead of opening a direct connection.
5. `main` reuses one DAX client through healthy, fault, and recovery phases with
   retries disabled. Each phase prints counts; request errors include a diagnostic.
6. `finally` stops SDK discovery, deletes only this run's proxies, then restores
   the connector. SIGINT/SIGTERM request graceful shutdown after the current call.

The original DAX endpoint is deliberately preserved. Replacing only its seed URL
with localhost would not capture later connections to discovered nodes.

## Limits And Troubleshooting

The adapter uses **internal** SDK fields `_protocol` and `_connectOps`; keep the
package pinned and rerun both tests and self-test after changing it. Run this app
as a dedicated process, not inside an existing server with another DAX client.
Only plaintext IPv4 DAX on port 8111 is covered. SDK timers also include time
spent preparing a new mapping; raise the baseline timeout if setup is too slow.

`main.test.js` checks fail-closed control errors without a server. The self-test
checks real Toxiproxy wiring, but neither proves real DAX authentication or fault
response. No live AWS test has been run for this example. See the shared guide
for how to inspect counts, mappings, metrics, and cleanup.