# Client Integration

This project owns the official Toxiproxy server lifecycle and its language-neutral HTTP control helpers. Applications own their client-specific connection routing.

## Generic TCP Client

1. Start Toxiproxy with `scripts/server.sh start` and keep the API bound to loopback.
2. Create a mapping using `scripts/toxiproxyctl.sh create <name> <loopback-listen> <service-upstream>`.
3. Configure the application's TCP client to connect to the mapping's listener.
4. Add/remove toxics with `scripts/toxiproxyctl.sh toxic-add` and `toxic-remove`.
5. Delete only the mappings created by the test, then stop the Toxiproxy process owned by its run directory.

Any language that can open TCP connections and send HTTP/JSON requests can use this workflow. No application needs to import a Go package from this repository.

## DAX Discovery

A DAX cluster seed endpoint can return multiple node endpoints. Proxying only the seed connection is insufficient: the DAX client may connect directly to discovered nodes. The DAX integration must resolve/create a loopback mapping per node and route each SDK dial through the corresponding listener. Keep this hook in the DAX client/test project so this repository does not take a dependency on any DAX SDK or language.

The client should fail closed if a node has no proxy mapping; never silently fall back to a direct connection during a fault test. Record node-to-proxy mapping verification before interpreting outcomes.

Toxiproxy latency/reset toxics affect data on a connection after TCP is accepted. They do not simulate a delayed client-to-proxy TCP handshake.

## Isolation And Cleanup

Use one Toxiproxy process, API port, PID/log directory, and proxy-name prefix per test invocation. Bind the control API to loopback. When possible, allocate proxy listener ports dynamically and use the actual port returned by the API. Clean up only mappings owned by the test, then stop only the process it started. Avoid the global `/reset` endpoint on a shared server because it changes every proxy on that instance.
