using System.Net;
using System.Net.Sockets;
using System.Reflection;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Amazon;
using Amazon.DAX;
using Amazon.DynamoDBv2.Model;
using Amazon.Runtime;
using HarmonyLib;

// A read-only DAX fault demonstration, not a production transport adapter.
// HTTP controls Toxiproxy; the SDK's binary protocol uses mapped TCP listeners.
// See README.md for VPC access, credentials, table keys, and compatibility limits.
internal static class Program
{
    internal static readonly JsonSerializerOptions Json = new() { PropertyNamingPolicy = JsonNamingPolicy.CamelCase };
    internal sealed record Proxy(string Name, string Listen, string Upstream, bool Enabled);

    /// <summary>
    /// Owns only this run's proxy names. Serialize discovery with phase transitions
    /// so new nodes inherit the active toxic before their sockets are connected.
    /// An HTTP/mapping failure is retained; never continue with uncertain coverage.
    /// </summary>
    internal sealed class Router
    {
        private readonly Uri api;
        private readonly HttpClient http = new(new HttpClientHandler { AllowAutoRedirect = false }) { Timeout = TimeSpan.FromSeconds(5) };
        private readonly SemaphoreSlim gate = new(1, 1);
        private readonly string prefix = $"dotnet-dax-{Environment.ProcessId}-{Guid.NewGuid():N}";
        internal readonly Dictionary<string, Proxy> Routes = new();
        private int latency;
        private Exception? failure;
        private bool closed;

        internal Router(string address)
        {
            api = new Uri(address);
            if (api.Scheme != "http" || api.Host != "127.0.0.1" || api.Port < 1 || api.AbsolutePath != "/"
                || api.UserInfo != "" || api.Query != "" || api.Fragment != "")
                throw new ArgumentException("TOXIPROXY_API must be http://127.0.0.1:PORT");
        }

        internal async Task<JsonNode?> Request(string method, string path, object? body = null, CancellationToken token = default)
        {
            using var request = new HttpRequestMessage(new HttpMethod(method), new Uri(api, path));
            if (body != null) request.Content = new StringContent(JsonSerializer.Serialize(body, Json), Encoding.UTF8, "application/json");
            using var response = await http.SendAsync(request, token);
            response.EnsureSuccessStatusCode();
            string payload = await response.Content.ReadAsStringAsync(token);
            return string.IsNullOrWhiteSpace(payload) ? null : JsonNode.Parse(payload);
        }

        private Task<JsonNode?> Toxic(Proxy mapping, int delay, CancellationToken token) => Request("POST", $"/proxies/{mapping.Name}/toxics", new
        {
            name = "example_latency", type = "latency", stream = "downstream", toxicity = 1,
            attributes = new { latency = delay, jitter = 0 }
        }, token);

        internal async Task<IPEndPoint> Mapping(HostPort destination, CancellationToken token)
        {
            await gate.WaitAsync(token);
            try
            {
                if (closed) throw new InvalidOperationException("Router closed");
                if (failure != null) throw new InvalidOperationException("Earlier routing failure", failure);
                try
                {
                    string host = destination.IpAddress?.ToString() ?? destination.HostName;
                    if (destination.Port != 8111 || string.IsNullOrWhiteSpace(host) || host.Contains(':'))
                        throw new ArgumentException("Only plaintext IPv4 DAX on port 8111 is supported");
                    string upstream = $"{host}:8111";
                    if (!Routes.TryGetValue(upstream, out var mapping))
                    {
                        var wanted = new Proxy($"{prefix}-{Routes.Count}", "127.0.0.1:0", upstream, true);
                        // Retain the name even if POST succeeds but its response is lost.
                        Routes[upstream] = wanted;
                        var response = await Request("POST", "/proxies", wanted, token);
                        mapping = response?.Deserialize<Proxy>(Json) ?? throw new InvalidOperationException("Missing proxy response");
                        if (mapping.Name != wanted.Name || mapping.Upstream != upstream || !mapping.Enabled
                            || !IPEndPoint.TryParse(mapping.Listen, out var listener) || !listener.Address.Equals(IPAddress.Loopback) || listener.Port == 0)
                            throw new InvalidOperationException("Invalid proxy mapping");
                        Routes[upstream] = mapping;
                        if (latency > 0) await Toxic(mapping, latency, token);
                        Console.WriteLine($"route {mapping.Name}: {mapping.Listen} -> {upstream}");
                    }
                    return IPEndPoint.Parse(mapping.Listen);
                }
                catch (Exception error) { failure = error; throw; }
            }
            finally { gate.Release(); }
        }

        internal async Task SetLatency(int delay, CancellationToken token)
        {
            await gate.WaitAsync(token);
            try
            {
                if (failure != null) throw new InvalidOperationException("Earlier routing failure", failure);
                try
                {
                    foreach (var mapping in Routes.Values)
                    {
                        if (delay > 0) await Toxic(mapping, delay, token);
                        else if (latency > 0) await Request("DELETE", $"/proxies/{mapping.Name}/toxics/example_latency", token: token);
                    }
                    latency = delay;
                }
                catch (Exception error) { failure = error; throw; }
            }
            finally { gate.Release(); }
        }

        internal async Task Close()
        {
            await gate.WaitAsync();
            try
            {
                closed = true;
                var errors = new List<Exception>();
                foreach (var mapping in Routes.Values)
                {
                    // Do not reuse a canceled run token: cleanup must still be attempted.
                    try { await Request("DELETE", $"/proxies/{mapping.Name}"); }
                    catch (Exception error) { errors.Add(error); }
                }
                if (errors.Count > 0) throw new AggregateException("Proxy cleanup failed", errors);
            }
            finally { gate.Release(); http.Dispose(); }
        }
    }

    /// <summary>
    /// The pinned SDK has no public dial hook. Harmony replaces only the internal
    /// TcpClientTubeFactory.ConnectTcpClientAsync method in this example process.
    /// It does not patch all TcpClient instances or affect credential HTTP calls.
    /// Keep the package pinned, do not use trimming/NativeAOT, and do not copy this
    /// into production. A missing target method causes startup to fail, not bypass.
    /// </summary>
    internal static class RoutingPatch
    {
        internal static Router Router = null!;
        internal static int ConnectTimeout = 2000;
        internal static readonly Type Factory = typeof(ClusterDaxClient).Assembly.GetType("Amazon.DAX.Internal.TcpClientTubeFactory", throwOnError: true)!;
        internal static readonly MethodInfo Target = AccessTools.Method(Factory, "ConnectTcpClientAsync",
            new[] { typeof(TcpClient), typeof(HostPort), typeof(CancellationToken) })
            ?? throw new NotSupportedException("The pinned SDK connector signature changed");

        // Return false to skip the original upstream connect. Supply our own Task
        // so the rest of the SDK still performs its normal protocol/authentication.
        internal static bool Prefix(object[] __args, ref Task __result)
        {
            __result = Connect((TcpClient)__args[0], (HostPort)__args[1], (CancellationToken)__args[2]);
            return false;
        }

        private static async Task Connect(TcpClient client, HostPort destination, CancellationToken token)
        {
            using var deadline = CancellationTokenSource.CreateLinkedTokenSource(token);
            deadline.CancelAfter(ConnectTimeout);
            var listener = await Router.Mapping(destination, deadline.Token);
            await client.ConnectAsync(listener.Address, listener.Port, deadline.Token);
        }
    }

    internal static Dictionary<string, AttributeValue> Key(string input)
    {
        var typed = JsonSerializer.Deserialize<Dictionary<string, Dictionary<string, string>>>(input)
            ?? throw new ArgumentException("Key must be a JSON object");
        if (typed.Count == 0) throw new ArgumentException("Key must not be empty");
        var result = new Dictionary<string, AttributeValue>();
        foreach (var (name, value) in typed)
        {
            if (string.IsNullOrEmpty(name) || value == null || value.Count != 1) throw new ArgumentException("Each key needs one S/N value");
            var attribute = value.Single();
            result[name] = attribute.Key switch
            {
                "S" => new AttributeValue { S = attribute.Value },
                "N" => new AttributeValue { N = attribute.Value },
                _ => throw new ArgumentException("This example accepts S and N key types only")
            };
        }
        return result;
    }

    private static int Positive(string name, int fallback)
    {
        int value = int.Parse(Environment.GetEnvironmentVariable(name) ?? fallback.ToString());
        return value > 0 ? value : throw new ArgumentException($"{name} must be positive");
    }

    public static async Task<int> Main(string[] args)
    {
        try { await Run(args); return 0; }
        catch (Exception error) { Console.Error.WriteLine(error); return 1; }
    }

    private static async Task Run(string[] args)
    {
        using var stopping = new CancellationTokenSource();
        ConsoleCancelEventHandler interrupt = (_, eventArgs) => { eventArgs.Cancel = true; stopping.Cancel(); };
        Console.CancelKeyPress += interrupt;
        using var term = OperatingSystem.IsWindows() ? null : PosixSignalRegistration.Create(PosixSignal.SIGTERM, context => { context.Cancel = true; stopping.Cancel(); });
        var router = new Router(Environment.GetEnvironmentVariable("TOXIPROXY_API") ?? "http://127.0.0.1:18474");
        RoutingPatch.Router = router;
        var harmony = new Harmony("example.toxiproxy.dax");
        ClusterDaxClient? client = null;
        bool patched = false;
        try
        {
            // Validate and patch before DAX construction starts background discovery.
            harmony.Patch(RoutingPatch.Target, prefix: new HarmonyMethod(typeof(RoutingPatch), nameof(RoutingPatch.Prefix)));
            patched = true;
            await router.Request("GET", "/version", token: stopping.Token);
            if (args.Contains("--self-test"))
            {
                // Invoke the SDK's actual patched method without AWS credentials.
                var factory = Activator.CreateInstance(RoutingPatch.Factory, TimeSpan.FromSeconds(2), TimeSpan.FromSeconds(2));
                using var tcp = new TcpClient();
                await (Task)RoutingPatch.Target.Invoke(factory, new object[] { tcp, new HostPort(IPAddress.Loopback, 8111), stopping.Token })!;
                await router.SetLatency(100, stopping.Token);
                await router.Mapping(new HostPort(IPAddress.Parse("127.0.0.2"), 8111), stopping.Token);
                await router.SetLatency(0, stopping.Token);
                if (router.Routes.Count != 2) throw new Exception("Expected two mappings");
                _ = Key("{\"id\":{\"N\":\"42\"}}");
                Console.WriteLine(".NET routing self-test passed");
                return;
            }
            string endpoint = Environment.GetEnvironmentVariable("DAX_ENDPOINT") ?? "";
            string table = Environment.GetEnvironmentVariable("TABLE_NAME") ?? "";
            if (!endpoint.StartsWith("dax://") || string.IsNullOrWhiteSpace(table)) throw new ArgumentException("Set DAX_ENDPOINT=dax://... and TABLE_NAME");
            var key = Key(Environment.GetEnvironmentVariable("DAX_KEY_JSON") ?? "{\"pk\":{\"S\":\"pk#1\"},\"sk\":{\"S\":\"sk#1\"}}");
            int timeout = Positive("DAX_TIMEOUT_MS", 200);
            int duration = Positive("DAX_DURATION_SECONDS", 10);
            int latency = Positive("DAX_LATENCY_MS", 1000);
            RoutingPatch.ConnectTimeout = timeout;
            var region = RegionEndpoint.GetBySystemName(Environment.GetEnvironmentVariable("AWS_REGION") ?? "eu-west-1");
            var config = new DaxClientConfig(endpoint)
            {
                RegionEndpoint = region,
                AwsCredentials = Amazon.Runtime.Credentials.DefaultAWSCredentialsIdentityResolver.GetCredentials(
                    new Amazon.DynamoDBv2.AmazonDynamoDBConfig { RegionEndpoint = region }),
                ReadRetryCount = 0, WriteRetryCount = 0,
                RequestTimeout = TimeSpan.FromMilliseconds(timeout), ConnectTimeout = TimeSpan.FromMilliseconds(timeout)
            };
            client = await ClusterDaxClient.CreateAsync(config, stopping.Token);
            foreach (string phase in new[] { "healthy", "fault", "recovery" })
            {
                stopping.Token.ThrowIfCancellationRequested();
                await router.SetLatency(phase == "fault" ? latency : 0, stopping.Token);
                int successes = 0, failures = 0;
                string? lastError = null;
                var elapsed = System.Diagnostics.Stopwatch.StartNew();
                while (elapsed.Elapsed < TimeSpan.FromSeconds(duration) && !stopping.IsCancellationRequested)
                {
                    using var request = CancellationTokenSource.CreateLinkedTokenSource(stopping.Token);
                    request.CancelAfter(timeout);
                    try { await client.GetItemAsync(new GetItemRequest { TableName = table, Key = key }, request.Token); successes++; }
                    catch (Exception error) { failures++; lastError = error.Message; }
                    await Task.Delay(100, stopping.Token); // Sequential low-rate reads, no writes.
                }
                Console.WriteLine(JsonSerializer.Serialize(new { phase, successes, failures, last_error = lastError }));
                if (phase == "healthy" && (successes == 0 || (double)successes / (successes + failures) < 0.95)) throw new Exception("Healthy prerequisite failed");
                if (phase == "recovery" && successes == 0) throw new Exception("No recovery progress");
            }
        }
        finally
        {
            // Close SDK background activity before removing listeners. Keep the
            // patch active until the router blocks any late connection attempts.
            try { client?.Dispose(); }
            finally
            {
                try { await router.Close(); }
                finally
                {
                    if (patched) harmony.Unpatch(RoutingPatch.Target, HarmonyPatchType.Prefix, harmony.Id);
                    Console.CancelKeyPress -= interrupt;
                }
            }
        }
    }
}