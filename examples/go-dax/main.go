// Command go-dax demonstrates a read-only DAX fault test using a running
// Toxiproxy server. Run the client and proxy on a host that can reach the cluster.
//
// The two network paths serve different purposes:
//
//   - Control: this app sends HTTP requests to Toxiproxy to manage proxies/toxics.
//   - Data: the DAX SDK opens TCP connections through those proxies to DAX nodes.
//
// A toxic is Toxiproxy's name for an injected network fault. This example adds
// downstream latency between a healthy baseline and a recovery phase. It does
// not start the server, create tables, write items, or simulate TCP handshake delay.
//
// Start the server using the toolkit scripts, then run from this directory:
//
//	go run . -endpoint dax://YOUR_CLUSTER -table YOUR_TABLE \
//	  -key '{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}'
//
// See ../../docs/go-dax-test.md for prerequisites, setup, and result inspection.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-dax-go-v2/dax"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// proxy describes a local TCP listener and the DAX endpoint it forwards to.
type proxy struct {
	// Name is also the API resource identifier used to remove this run's proxy.
	Name string `json:"name"`
	// Listen is requested as 127.0.0.1:0, then replaced with the allocated address.
	Listen string `json:"listen"`
	// Upstream remains the original seed/node address supplied by the DAX SDK.
	Upstream string `json:"upstream"`
	// Enabled must be true before the SDK can connect through this listener.
	Enabled bool `json:"enabled"`
}

// router keeps SDK discovery traffic and node connections behind Toxiproxy.
// The mutex serializes mapping creation with fault changes and cleanup.
type router struct {
	// SDK discovery and connection-pool goroutines may call dial concurrently.
	mutex sync.Mutex
	// api and client are used only for Toxiproxy's HTTP control interface.
	api    string
	client *http.Client
	// A per-run prefix keeps cleanup separate from other users of the server.
	prefix string
	// routes is keyed by the original DAX host:port, not the local listener.
	routes map[string]proxy
	// latency is the fault to apply to any nodes discovered later in the phase.
	latency time.Duration
	// closed prevents SDK background activity from creating proxies during cleanup.
	closed bool
	// failure retains the first control/mapping error so later dials cannot silently
	// continue an experiment whose routing or fault coverage is uncertain.
	failure error
}

// newRouter prepares control-plane access without creating proxies or dialing DAX.
// The local Toxiproxy server must already be running; run checks it before AWS setup.
func newRouter(api string) (*router, error) {
	// The admin API can alter every proxy; keep it local and reject redirects.
	parsed, err := url.Parse(api)
	if err != nil || parsed.Scheme != "http" || !net.ParseIP(parsed.Hostname()).IsLoopback() || parsed.Port() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("proxy API must be an HTTP loopback IP origin, such as http://127.0.0.1:18474")
	}
	return &router{
		api: api, prefix: fmt.Sprintf("go-dax-%d-%d", os.Getpid(), time.Now().UnixNano()),
		// The HTTP timeout bounds control calls even when their context has no deadline.
		client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		routes: make(map[string]proxy),
	}, nil
}

// request sends control-plane HTTP requests, not the DAX application's TCP traffic.
// A nil input means no JSON payload; a nil output means no response decoding.
// Callers provide the context so startup, phase changes, and cleanup can use
// different cancellation policies while sharing the same HTTP implementation.
func (route *router) request(ctx context.Context, method, path string, input, output any) error {
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, route.api+path, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := route.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Include a bounded API error body for diagnosis without reading it unboundedly.
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Toxiproxy %s %s: status %d: %s", method, path, response.StatusCode, detail)
	}
	if output != nil {
		// Proxy descriptions are small; cap decoding rather than trust the server's size.
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
	}
	return nil
}

// addLatency installs this example's named toxic on one proxy. A fixed toxic name
// lets recovery remove just this fault without resetting unrelated server state.
func (route *router) addLatency(ctx context.Context, mapping proxy, latency time.Duration) error {
	// Downstream delays bytes returning to the client after TCP is established.
	// Toxicity 1 applies the toxic fully; zero jitter keeps the configured delay fixed.
	return route.request(ctx, http.MethodPost, "/proxies/"+mapping.Name+"/toxics", map[string]any{
		"name": "example_latency", "type": "latency", "stream": "downstream", "toxicity": 1,
		"attributes": map[string]int64{"latency": latency.Milliseconds(), "jitter": 0},
	}, nil)
}

// dial is the SDK hook for both the seed endpoint and discovered node endpoints.
// A mapping error must fail the dial, never fall back to a direct DAX connection.
func (route *router) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("unsupported network %q", network)
	}
	// This example supports plaintext DAX on 8111, not TLS or local proxy chaining.
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port != "8111" || host == "localhost" || net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("unexpected plaintext DAX destination %q", address)
	}
	mapping, err := route.mapping(ctx, address)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	// The SDK's context may impose an earlier deadline than the dialer's timeout.
	// This establishes the client-to-proxy socket; DAX authentication happens later.
	return dialer.DialContext(ctx, network, mapping.Listen)
}

// mapping lazily creates one proxy per destination and reuses it for later sockets.
// Proxying only the seed would miss connections to nodes returned by DAX discovery.
func (route *router) mapping(ctx context.Context, address string) (proxy, error) {
	// Hold the lock across API calls so concurrent dials cannot create duplicates or
	// race with a phase transition before the new proxy receives its active toxic.
	route.mutex.Lock()
	defer route.mutex.Unlock()
	if route.closed {
		return proxy{}, errors.New("proxy router closed")
	}
	if route.failure != nil {
		return proxy{}, route.failure
	}
	if mapping, exists := route.routes[address]; exists {
		return mapping, nil
	}
	// Port zero lets Toxiproxy allocate a free listener without a port-selection race.
	wanted := proxy{Name: fmt.Sprintf("%s-%d", route.prefix, len(route.routes)), Listen: "127.0.0.1:0", Upstream: address, Enabled: true}
	// Track the name before POST: the server may create it even if the response is lost.
	route.routes[address] = wanted
	var created proxy
	if err := route.request(ctx, http.MethodPost, "/proxies", wanted, &created); err != nil {
		route.failure = err
		return proxy{}, err
	}
	// Use only a validated loopback listener returned for our original destination.
	listenHost, listenPort, err := net.SplitHostPort(created.Listen)
	if err != nil || !net.ParseIP(listenHost).IsLoopback() || listenPort == "" || listenPort == "0" || created.Name != wanted.Name || created.Upstream != address || !created.Enabled {
		route.failure = fmt.Errorf("invalid proxy mapping: %+v", created)
		return proxy{}, route.failure
	}
	route.routes[address] = created
	// Nodes discovered mid-fault must receive the same toxic before their first dial.
	if route.latency > 0 {
		if err := route.addLatency(ctx, created, route.latency); err != nil {
			route.failure = err
			return proxy{}, err
		}
	}
	fmt.Printf("route %s -> %s -> %s\n", created.Name, created.Listen, created.Upstream)
	return created, nil
}

// setLatency changes existing proxies and records the setting for future mappings.
// Zero removes this example's toxic for the recovery phase.
// The test calls this once per phase: zero, the configured delay, then zero.
func (route *router) setLatency(ctx context.Context, latency time.Duration) error {
	route.mutex.Lock()
	defer route.mutex.Unlock()
	if route.failure != nil {
		return route.failure
	}
	for _, mapping := range route.routes {
		var err error
		if latency > 0 {
			err = route.addLatency(ctx, mapping, latency)
		} else if route.latency > 0 {
			err = route.request(ctx, http.MethodDelete, "/proxies/"+mapping.Name+"/toxics/example_latency", nil, nil)
		}
		if err != nil {
			// Some proxies may already have changed. Abort rather than measure a phase
			// with only partial fault coverage; close will still attempt their deletion.
			route.failure = err
			return err
		}
	}
	// Publish the new setting only after every existing proxy has been updated.
	route.latency = latency
	return nil
}

// close deletes only proxies named by this run, never globally resets the server.
func (route *router) close() error {
	route.mutex.Lock()
	defer route.mutex.Unlock()
	route.closed = true
	var failures []error
	for _, mapping := range route.routes {
		// Cleanup must still work after Ctrl+C cancels the application's context.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := route.request(ctx, http.MethodDelete, "/proxies/"+mapping.Name, nil, nil)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	// Try every deletion and retain all failures instead of stopping at the first.
	return errors.Join(failures...)
}

// phaseResult is emitted as one JSON line after each phase. Failures include any
// GetItem error, not just timeouts; LastError provides one diagnostic sample.
// A successful request may return no item, which is still a valid DAX response.
type phaseResult struct {
	Phase     string `json:"phase"`
	Successes int    `json:"successes"`
	Failures  int    `json:"failures"`
	LastError string `json:"last_error,omitempty"`
}

// run owns the whole experiment so its deferred cleanup runs before main exits.
// The named return lets cleanup errors be joined with the original experiment error.
func run() (runErr error) {
	// Defaults keep load low. Match -key to the existing table's actual key schema.
	// -duration is per phase, while -request-timeout bounds one GetItem operation.
	api := flag.String("proxy-api", "http://127.0.0.1:18474", "Toxiproxy loopback control API")
	endpoint := flag.String("endpoint", "", "plaintext dax:// cluster endpoint")
	region := flag.String("region", "eu-west-1", "AWS region")
	table := flag.String("table", "", "existing test table")
	keyJSON := flag.String("key", `{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}`, "DynamoDB key JSON, with S, N, or B attribute values")
	duration := flag.Duration("duration", 10*time.Second, "duration of each phase")
	timeout := flag.Duration("request-timeout", 200*time.Millisecond, "per-request deadline")
	interval := flag.Duration("interval", 100*time.Millisecond, "minimum interval between sequential requests")
	latency := flag.Duration("latency", time.Second, "downstream latency during fault phase")
	flag.Parse()
	// Reject unsupported settings before creating any proxies. Toxiproxy expects
	// latency in whole milliseconds, and this example does not configure DAX TLS.
	parsed, err := url.Parse(*endpoint)
	if err != nil || parsed.Scheme != "dax" || parsed.Hostname() == "" || *table == "" || *duration <= 0 || *timeout <= 0 || *interval <= 0 || *latency < time.Millisecond || *latency%time.Millisecond != 0 {
		return errors.New("provide -endpoint dax://..., -table, positive timing values, and whole-millisecond latency; TLS is not supported")
	}
	key, err := parseKey(*keyJSON)
	if err != nil {
		return err
	}
	// Signals cancel requests; deferred cleanup closes DAX before deleting its proxies.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	routing, err := newRouter(*api)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, routing.close()) }()
	// Fail early if the control API is unavailable, before loading credentials or
	// starting the SDK's discovery and connection-pool activity.
	if err := routing.request(ctx, http.MethodGet, "/version", nil, nil); err != nil {
		return err
	}
	// Use the normal AWS credential chain (for example, a profile or EC2 role).
	// Toxiproxy forwards the SDK's DAX protocol traffic; it does not sign requests.
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(*region))
	if err != nil {
		return err
	}
	// Preserve the real endpoint for discovery; intercept connections via DialContext.
	cfg := dax.NewConfig(awsConfig, *endpoint)
	cfg.RequestTimeout = *timeout
	// Disable SDK retries so they do not obscure the injected fault's effect.
	cfg.ReadRetries, cfg.WriteRetries = 0, 0
	cfg.DialContext = routing.dial
	client, err := dax.New(cfg)
	if err != nil {
		return err
	}
	// Defers run in reverse order: close the SDK first, then delete its proxies.
	// This avoids removing listeners while the client is still using them.
	defer func() { runErr = errors.Join(runErr, client.Close()) }()
	// Establish a healthy baseline, inject response latency, then test recovery.
	for _, phase := range []string{"healthy", "fault", "recovery"} {
		delay := time.Duration(0)
		if phase == "fault" {
			delay = *latency
		}
		// Change toxics on the same client's connections, rather than reconstructing
		// the SDK between phases and hiding how its pools respond to the fault.
		if err := routing.setLatency(ctx, delay); err != nil {
			return err
		}
		result := phaseResult{Phase: phase}
		phaseCtx, cancelPhase := context.WithTimeout(ctx, *duration)
		ticker := time.NewTicker(*interval)
		// Calls are sequential; slow responses reduce the offered request rate.
		for phaseCtx.Err() == nil {
			select {
			case <-phaseCtx.Done():
			case <-ticker.C:
				// Each request is bounded by both its own deadline and the phase deadline.
				requestCtx, cancel := context.WithTimeout(phaseCtx, *timeout)
				_, err := client.GetItem(requestCtx, &dynamodb.GetItemInput{TableName: table, Key: key})
				// Release the per-request timer promptly rather than defer inside the loop.
				cancel()
				if phaseCtx.Err() != nil {
					// Do not count phase shutdown as an injected-fault failure.
					continue
				}
				if err != nil {
					result.Failures++
					result.LastError = err.Error()
				} else {
					result.Successes++
				}
			}
		}
		ticker.Stop()
		cancelPhase()
		// Route messages are plain text; phase summaries are JSON lines on stdout.
		// The walkthrough shows how to filter the JSON when inspecting a saved log.
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Do not interpret fault results unless the baseline was already working.
		if phase == "healthy" && (result.Successes == 0 || float64(result.Successes)/float64(result.Successes+result.Failures) < 0.95) {
			return errors.New("healthy phase requires at least 95% success; check credentials, table key, connectivity, and deadline")
		}
		// Recovery must show some progress. We do not demand a particular fault-phase
		// failure rate: a zero exit alone is not proof that every request saw a timeout.
		if phase == "recovery" && result.Successes == 0 {
			return errors.New("no successful requests during recovery")
		}
	}
	return nil
}

// parseKey accepts DynamoDB's typed JSON key format, for example {"id":{"N":"42"}}.
// Only scalar S, N, and B types can be used for partition and sort keys.
func parseKey(input string) (map[string]types.AttributeValue, error) {
	// Preserve each typed value as raw JSON until its S/N/B tag selects a decoder.
	// The key's attribute names belong to the user's table, so they are not hardcoded.
	var attributes map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &attributes); err != nil {
		return nil, fmt.Errorf("invalid key JSON: %w", err)
	}
	key := make(map[string]types.AttributeValue)
	for name, attribute := range attributes {
		if name == "" || len(attribute) != 1 {
			return nil, errors.New("each key attribute needs a name and exactly one S, N, or B value")
		}
		for kind, raw := range attribute {
			switch kind {
			case "S", "N":
				// DynamoDB's JSON format represents both strings and numbers as JSON
				// strings. The SDK/service handles numeric-value and schema validation.
				var value string
				if err := json.Unmarshal(raw, &value); err != nil || value == "" {
					return nil, fmt.Errorf("key %s must contain a nonempty string", name)
				}
				if kind == "S" {
					key[name] = &types.AttributeValueMemberS{Value: value}
				} else {
					key[name] = &types.AttributeValueMemberN{Value: value}
				}
			case "B":
				// encoding/json decodes a base64 JSON string into the binary key bytes.
				var value []byte
				if err := json.Unmarshal(raw, &value); err != nil || len(value) == 0 {
					return nil, fmt.Errorf("key %s must contain a nonempty base64 string", name)
				}
				key[name] = &types.AttributeValueMemberB{Value: value}
			default:
				return nil, fmt.Errorf("unsupported key type %q", kind)
			}
		}
	}
	if len(key) == 0 {
		return nil, errors.New("key must contain at least one attribute")
	}
	return key, nil
}

// main handles process exit only after run has returned and executed its defers.
// Calling os.Exit inside run would skip the client and proxy cleanup.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
