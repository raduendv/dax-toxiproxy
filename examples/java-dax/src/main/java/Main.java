import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import io.netty.bootstrap.Bootstrap;
import io.netty.channel.Channel;
import io.netty.channel.ChannelFuture;
import io.netty.channel.ChannelHandlerContext;
import io.netty.channel.ChannelOutboundHandlerAdapter;
import io.netty.channel.ChannelPromise;
import io.netty.channel.nio.NioEventLoopGroup;
import io.netty.channel.socket.nio.NioSocketChannel;
import software.amazon.dax.ClusterDaxClient;
import software.amazon.dax.Configuration;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.dynamodb.model.AttributeValue;
import software.amazon.awssdk.services.dynamodb.model.GetItemRequest;

import java.net.InetSocketAddress;
import java.net.SocketAddress;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Read-only DAX/Toxiproxy demonstration. HTTP configures faults; DAX bytes use
 * TCP listeners. The original cluster URL is preserved for discovery, while a
 * Netty outbound handler redirects every seed/node connect to a mapped proxy.
 * This is a test harness, not a production networking component. See README.md.
 */
public final class Main {
    static final ObjectMapper JSON = new ObjectMapper();

    /** Owns this run's proxy names and serializes discovery with fault changes. */
    static final class Router implements AutoCloseable {
        final URI api;
        final HttpClient http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(5))
                .followRedirects(HttpClient.Redirect.NEVER).build();
        final String prefix = "java-dax-" + ProcessHandle.current().pid() + "-" + System.nanoTime();
        final Map<String, JsonNode> routes = new LinkedHashMap<>();
        int latency;
        Exception failure;
        boolean closed;

        Router(String address) {
            api = URI.create(address);
            if (!"http".equals(api.getScheme()) || !"127.0.0.1".equals(api.getHost()) || api.getPort() < 1
                    || api.getRawUserInfo() != null || api.getQuery() != null || api.getFragment() != null
                    || (!api.getPath().isEmpty() && !api.getPath().equals("/"))) {
                throw new IllegalArgumentException("TOXIPROXY_API must be http://127.0.0.1:PORT");
            }
        }

        JsonNode request(String method, String path, Object body) throws Exception {
            var payload = body == null ? HttpRequest.BodyPublishers.noBody()
                    : HttpRequest.BodyPublishers.ofString(JSON.writeValueAsString(body));
            var request = HttpRequest.newBuilder(api.resolve(path)).timeout(Duration.ofSeconds(5))
                    .header("Content-Type", "application/json").method(method, payload).build();
            var response = http.send(request, HttpResponse.BodyHandlers.ofString());
            if (response.statusCode() < 200 || response.statusCode() >= 300) {
                throw new IllegalStateException(method + " " + path + ": " + response.statusCode());
            }
            return response.body().isBlank() ? null : JSON.readTree(response.body());
        }

        void toxic(JsonNode mapping, int delay) throws Exception {
            request("POST", "/proxies/" + mapping.path("name").asText() + "/toxics", Map.of(
                    "name", "example_latency", "type", "latency", "stream", "downstream", "toxicity", 1,
                    "attributes", Map.of("latency", delay, "jitter", 0)));
        }

        synchronized InetSocketAddress mapping(InetSocketAddress destination) throws Exception {
            if (closed) throw new IllegalStateException("router closed");
            if (failure != null) throw new IllegalStateException("Earlier routing failure", failure);
            try {
                String host = destination.getHostString();
                if (destination.getPort() != 8111 || host.contains(":")) {
                    throw new IllegalArgumentException("Only plaintext IPv4 DAX on port 8111 is supported");
                }
                String upstream = host + ":8111";
                JsonNode mapping = routes.get(upstream);
                if (mapping == null) {
                    var wanted = Map.of("name", prefix + "-" + routes.size(), "listen", "127.0.0.1:0",
                            "upstream", upstream, "enabled", true);
                    // Record the name before POST so an uncertain response can still be cleaned up.
                    routes.put(upstream, JSON.valueToTree(wanted));
                    mapping = request("POST", "/proxies", wanted);
                    String listen = mapping.path("listen").asText();
                    if (!mapping.path("name").asText().equals(wanted.get("name"))
                            || !mapping.path("upstream").asText().equals(upstream)
                            || !mapping.path("enabled").asBoolean()
                            || !listen.matches("127\\.0\\.0\\.1:[1-9][0-9]*")) {
                        throw new IllegalStateException("Invalid proxy mapping");
                    }
                    routes.put(upstream, mapping);
                    // Apply the active fault before allowing a newly discovered node to connect.
                    if (latency > 0) toxic(mapping, latency);
                    System.out.println("route " + mapping.path("name").asText() + ": " + listen + " -> " + upstream);
                }
                return new InetSocketAddress("127.0.0.1", Integer.parseInt(mapping.path("listen").asText().split(":")[1]));
            } catch (Exception error) {
                failure = error; // Never silently continue with incomplete routing/fault coverage.
                throw error;
            }
        }

        synchronized void setLatency(int delay) throws Exception {
            if (failure != null) throw new IllegalStateException("Earlier routing failure", failure);
            try {
                for (JsonNode mapping : routes.values()) {
                    if (delay > 0) toxic(mapping, delay);
                    else if (latency > 0) request("DELETE", "/proxies/" + mapping.path("name").asText() + "/toxics/example_latency", null);
                }
                latency = delay;
            } catch (Exception error) { failure = error; throw error; }
        }

        @Override public synchronized void close() throws Exception {
            closed = true;
            Exception failure = null;
            // Try every owned deletion. Never use /reset: it changes unrelated proxies.
            for (JsonNode mapping : routes.values()) {
                try { request("DELETE", "/proxies/" + mapping.path("name").asText(), null); }
                catch (Exception error) {
                    if (failure == null) failure = error;
                    else failure.addSuppressed(error);
                }
            }
            if (failure != null) throw failure;
        }
    }

    /**
     * Configuration.eventLoopGroup is public. Register a handler before the SDK
     * connects each channel. Outbound connect is redirected, not ordinary writes,
     * so DAX authentication/framing remains the official SDK's responsibility.
     */
    static final class ProxyEventLoops extends NioEventLoopGroup {
        private final Router router;
        ProxyEventLoops(Router router) { super(1); this.router = router; }

        @Override public ChannelFuture register(Channel channel) {
            channel.pipeline().addFirst("toxiproxy-connect", new ChannelOutboundHandlerAdapter() {
                @Override public void connect(ChannelHandlerContext context, SocketAddress remote,
                                              SocketAddress local, ChannelPromise promise) {
                    // Control HTTP may take seconds; never block the Netty event-loop thread.
                    CompletableFuture.supplyAsync(() -> {
                        try { return router.mapping((InetSocketAddress) remote); }
                        catch (Exception error) { throw new java.util.concurrent.CompletionException(error); }
                    }).whenComplete((mapped, error) -> context.executor().execute(() -> {
                        if (error != null) { promise.tryFailure(error); context.close(); }
                        else if (!promise.isDone() && context.channel().isOpen()) context.connect(mapped, local, promise);
                    }));
                }
            });
            return super.register(channel);
        }
    }

    static int positive(String name, int fallback) {
        int value = Integer.parseInt(System.getenv().getOrDefault(name, String.valueOf(fallback)));
        if (value <= 0) throw new IllegalArgumentException(name + " must be positive");
        return value;
    }

    static Map<String, AttributeValue> key(String input) throws Exception {
        JsonNode document = JSON.readTree(input);
        if (!document.isObject() || document.isEmpty()) throw new IllegalArgumentException("Key must be a nonempty object");
        Map<String, AttributeValue> key = new LinkedHashMap<>();
        var fields = document.fields();
        while (fields.hasNext()) {
            var field = fields.next();
            JsonNode value = field.getValue();
            if (value.size() != 1) throw new IllegalArgumentException("Each key needs exactly one S/N value");
            if (value.path("S").isTextual()) key.put(field.getKey(), AttributeValue.builder().s(value.get("S").asText()).build());
            else if (value.path("N").isTextual()) key.put(field.getKey(), AttributeValue.builder().n(value.get("N").asText()).build());
            else throw new IllegalArgumentException("This example accepts S and N key types only");
        }
        return key;
    }

    public static void main(String[] args) throws Exception {
        AtomicBoolean stopping = new AtomicBoolean();
        CountDownLatch finished = new CountDownLatch(1);
        // Give the main thread time to close DAX, event loops, and proxies on SIGTERM.
        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            stopping.set(true);
            try { finished.await(30, TimeUnit.SECONDS); }
            catch (InterruptedException error) { Thread.currentThread().interrupt(); }
        }));
        try (Router router = new Router(System.getenv().getOrDefault("TOXIPROXY_API", "http://127.0.0.1:18474"))) {
            router.request("GET", "/version", null);
            ProxyEventLoops loops = new ProxyEventLoops(router);
            try {
                if (args.length == 1 && args[0].equals("--self-test")) {
                    // Exercise the real Netty connect handler without signing DAX requests.
                    Channel channel = new Bootstrap().group(loops).channel(NioSocketChannel.class)
                            .handler(new io.netty.channel.ChannelInboundHandlerAdapter())
                            .connect("127.0.0.1", 8111).sync().channel();
                    channel.close().sync();
                    router.setLatency(100);
                    router.mapping(new InetSocketAddress("127.0.0.2", 8111));
                    router.setLatency(0);
                    if (router.routes.size() != 2) throw new AssertionError("Expected two mappings");
                    System.out.println("Java routing self-test passed");
                    return;
                }
                String endpoint = System.getenv().getOrDefault("DAX_ENDPOINT", "");
                String table = System.getenv("TABLE_NAME");
                if (!endpoint.startsWith("dax://") || table == null || table.isBlank()) throw new IllegalArgumentException("Set DAX_ENDPOINT=dax://... and TABLE_NAME");
                var key = key(System.getenv().getOrDefault("DAX_KEY_JSON", "{\"pk\":{\"S\":\"pk#1\"},\"sk\":{\"S\":\"sk#1\"}}"));
                int timeout = positive("DAX_TIMEOUT_MS", 200);
                int duration = positive("DAX_DURATION_SECONDS", 10);
                int latency = positive("DAX_LATENCY_MS", 1000);
                var configuration = Configuration.builder().url(endpoint)
                        .region(Region.of(System.getenv().getOrDefault("AWS_REGION", "eu-west-1")))
                        .readRetries(0).writeRetries(0).requestTimeoutMillis(timeout)
                        .connectTimeoutMillis(timeout).eventLoopGroup(loops).build();
                // The SDK handles normal AWS credentials and DAX protocol/authentication.
                try (var client = ClusterDaxClient.builder().overrideConfiguration(configuration).build()) {
                    for (String phase : new String[]{"healthy", "fault", "recovery"}) {
                        if (stopping.get()) throw new InterruptedException("Interrupted");
                        router.setLatency(phase.equals("fault") ? latency : 0);
                        int successes = 0, failures = 0;
                        String lastError = "";
                        long end = System.nanoTime() + TimeUnit.SECONDS.toNanos(duration);
                        while (System.nanoTime() < end && !stopping.get()) {
                            try { client.getItem(GetItemRequest.builder().tableName(table).key(key).build()); successes++; }
                            catch (Exception error) { failures++; lastError = error.toString(); }
                            Thread.sleep(100); // Low-rate sequential reads, not a throughput benchmark.
                        }
                        System.out.println(JSON.writeValueAsString(Map.of("phase", phase, "successes", successes, "failures", failures, "last_error", lastError)));
                        if (stopping.get()) throw new InterruptedException("Interrupted");
                        if (phase.equals("healthy") && (successes == 0 || (double) successes / (successes + failures) < 0.95)) throw new IllegalStateException("Healthy prerequisite failed");
                        if (phase.equals("recovery") && successes == 0) throw new IllegalStateException("No recovery progress");
                    }
                }
            } finally {
                // The SDK closes first; stop its event loops before deleting listeners.
                loops.shutdownGracefully(0, 5, TimeUnit.SECONDS).sync();
            }
        } finally { finished.countDown(); }
    }
}