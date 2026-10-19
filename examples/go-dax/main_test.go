package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoutingFaultsAndCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mutex sync.Mutex
	created := make(map[string]proxy)
	toxicAdds, toxicDeletes, proxyDeletes := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/proxies":
			var mapping proxy
			if err := json.NewDecoder(request.Body).Decode(&mapping); err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if mapping.Listen != "127.0.0.1:0" || !mapping.Enabled {
				t.Errorf("unexpected mapping: %+v", mapping)
			}
			mapping.Listen = listener.Addr().String()
			created[mapping.Upstream] = mapping
			json.NewEncoder(writer).Encode(mapping)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/toxics"):
			var toxic struct {
				Type       string `json:"type"`
				Stream     string `json:"stream"`
				Attributes struct {
					Latency int `json:"latency"`
				} `json:"attributes"`
			}
			if err := json.NewDecoder(request.Body).Decode(&toxic); err != nil || toxic.Type != "latency" || toxic.Stream != "downstream" || toxic.Attributes.Latency != 100 {
				t.Errorf("unexpected toxic: %+v, error: %v", toxic, err)
			}
			toxicAdds++
		case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/toxics/example_latency"):
			toxicDeletes++
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/proxies/go-dax-"):
			proxyDeletes++
		default:
			t.Errorf("unexpected API request: %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	routing, err := newRouter(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := routing.mapping(context.Background(), "node-one.example:8111"); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	connection, err := routing.dial(context.Background(), "tcp", "node-one.example:8111")
	if err != nil {
		t.Fatal(err)
	}
	if connection.RemoteAddr().String() != listener.Addr().String() {
		t.Errorf("dial bypassed proxy: %s", connection.RemoteAddr())
	}
	connection.Close()
	if err := routing.setLatency(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.mapping(context.Background(), "node-two.example:8111"); err != nil {
		t.Fatal(err)
	}
	if err := routing.setLatency(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := routing.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.mapping(context.Background(), "node-three.example:8111"); err == nil {
		t.Fatal("closed router accepted a mapping")
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(created) != 2 || toxicAdds != 2 || toxicDeletes != 2 || proxyDeletes != 2 {
		t.Fatalf("created=%d, toxic adds=%d, toxic deletes=%d, proxy deletes=%d", len(created), toxicAdds, toxicDeletes, proxyDeletes)
	}
}

func TestRoutingFailsClosed(t *testing.T) {
	for _, invalidMapping := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if !invalidMapping {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var mapping proxy
			json.NewDecoder(request.Body).Decode(&mapping)
			mapping.Listen = "192.0.2.1:1234"
			json.NewEncoder(writer).Encode(mapping)
		}))
		routing, err := newRouter(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		connection, err := routing.dial(context.Background(), "tcp", "node.example:8111")
		if connection != nil || err == nil {
			t.Fatalf("failed mapping did not fail closed: connection=%v error=%v", connection, err)
		}
		if err := routing.setLatency(context.Background(), time.Second); err == nil {
			t.Fatal("routing error was not retained")
		}
		server.Close()
	}
	for _, api := range []string{"http://192.0.2.1:8474", "https://127.0.0.1:8474", "http://127.0.0.1:8474/proxies"} {
		if _, err := newRouter(api); err == nil {
			t.Errorf("accepted unsafe API %s", api)
		}
	}
}

func TestParseKey(t *testing.T) {
	key, err := parseKey(`{"pk":{"S":"example"},"sk":{"N":"42"},"binary":{"B":"YWJj"}}`)
	if err != nil || len(key) != 3 {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, input := range []string{`{}`, `null`, `bad json`, `{"pk":{"BOOL":true}}`, `{"pk":{"S":""}}`, `{"pk":{"S":"x","N":"1"}}`, `{"pk":{"B":"!"}}`} {
		if _, err := parseKey(input); err == nil {
			t.Errorf("accepted invalid key %s", input)
		}
	}
}
