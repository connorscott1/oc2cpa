package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestBootstrapPreservesExistingCredentials(t *testing.T) {
	for _, file := range []string{
		`{"name":"oc2cpa.json","provider":"oc2cpa","disabled":true}`,
		`{"name":"custom.json","type":"oc2cpa"}`,
		`{"name":"oc2cpa.json","provider":"different-provider"}`,
	} {
		t.Run(file, func(t *testing.T) {
			saves := 0
			err := bootstrapAuth(func(method string, _ any) (json.RawMessage, error) {
				if method == pluginabi.MethodHostAuthSave {
					saves++
					return nil, nil
				}
				// Exercise the actual envelope decoder before passing the host result.
				return decodeHostResult([]byte(`{"ok":true,"result":{"files":[` + file + `]}}`))
			})
			if err != nil || saves != 0 {
				t.Fatalf("existing credential changed: saves=%d err=%v", saves, err)
			}
		})
	}
}

func TestBootstrapCreatesOnlyOnceAndFailsClosed(t *testing.T) {
	var files []pluginapi.HostAuthFileEntry = []pluginapi.HostAuthFileEntry{}
	saves := 0
	call := func(method string, value any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			raw, _ := json.Marshal(hostAuthListResponse{Files: files})
			return raw, nil
		}
		req := value.(pluginapi.HostAuthSaveRequest)
		if req.Name != "oc2cpa.json" {
			t.Errorf("unexpected file %s", req.Name)
		}
		files = append(files, pluginapi.HostAuthFileEntry{Name: req.Name, Provider: providerName})
		saves++
		return json.RawMessage(`{}`), nil
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := bootstrapAuth(call); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if saves != 1 {
		t.Fatalf("created %d times", saves)
	}
	for _, raw := range []string{`{}`, `null`, `{"files":null}`, `not-json`} {
		err := bootstrapAuth(func(method string, _ any) (json.RawMessage, error) {
			if method == pluginabi.MethodHostAuthSave {
				t.Fatal("saved after invalid list")
			}
			return json.RawMessage(raw), nil
		})
		if err == nil {
			t.Fatalf("accepted invalid list %s", raw)
		}
	}
	for _, raw := range []string{`{"ok":false,"result":{}}`, `{"ok":true}`, `{"ok":true,"error":{"message":"secret"}}`} {
		if _, err := decodeHostResult([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid envelope %s", raw)
		}
	}
}

func TestProxyValidationNeverLeaksOrFallsBack(t *testing.T) {
	for _, proxy := range []string{"ftp://user:PRIVATE@localhost:1", "socks6://user:PRIVATE@localhost:1", "http://", "http://u:PRIVATE@localhost/path", "http://localhost?token=PRIVATE", "%PRIVATE"} {
		client, err := buildClient(proxy, 100)
		if client != nil || err == nil {
			t.Fatalf("proxy accepted: %q", proxy)
		}
		if strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("proxy secret leaked")
		}
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		client, err := buildClient(scheme+"://user:PRIVATE@localhost:1080", 100)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := client.Transport.(*http.Transport).Proxy(&http.Request{})
		if err != nil || proxy.Scheme != scheme {
			t.Fatalf("proxy not honored: %v %v", proxy, err)
		}
		client.CloseIdleConnections()
	}
}

func TestRedirectDoesNotForwardConversation(t *testing.T) {
	var leaked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(307)
	}))
	defer redirect.Close()
	client, err := buildClient("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	_, err = client.Post(redirect.URL, "application/json", strings.NewReader(`{"messages":["private"]}`))
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("redirect forwarded: leaked=%d err=%v", leaked.Load(), err)
	}
}

func TestTotalDeadlineIncludesResponseBody(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
	}))
	defer srv.Close()
	client, _ := buildClient("", 1000)
	client.Timeout = 100 * time.Millisecond
	defer client.CloseIdleConnections()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err = io.ReadAll(resp.Body); err == nil {
		t.Fatal("body read outlived total deadline")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not observe cancellation")
	}
}

func TestResponseSizeLimits(t *testing.T) {
	body := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader("12345")), remaining: 4}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("oversized body accepted")
	}
	if _, err := body.Read(make([]byte, 1)); err == nil {
		t.Fatal("oversized reader recovered silently")
	}
	if _, err := readAllBounded(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversized catalog accepted")
	}
	if data, err := readAllBounded(strings.NewReader("1234"), 4); err != nil || string(data) != "1234" {
		t.Fatal("exact limit rejected")
	}
	for _, data := range []string{strings.Repeat("x", maxEventBytes+1), strings.Repeat("data: "+strings.Repeat("x", 1024)+"\n", maxEventBytes/1024+1)} {
		_, err := aggregateUpstreamJSON(io.NopCloser(strings.NewReader(data)), baseTestCtx())
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("oversized SSE accepted: %v", err)
		}
	}
}

func TestDiscoveryDoesNotBlockReloadOrPublishStaleCatalog(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"data":[{"id":"stale-free"}]}`)
	}))
	defer srv.Close()
	defer close(release)
	old := readSettings()
	defer func() { settingsMu.Lock(); settings = old; settingsMu.Unlock(); invalidateModelCache() }()
	settingsMu.Lock()
	settings = pluginSettings{UpstreamBaseURL: srv.URL, ConnectTimeoutMS: 1000, RequestTimeoutMS: 5000, DynamicModels: true}
	settingsMu.Unlock()
	invalidateModelCache()
	finished := make(chan []pluginapi.ModelInfo, 1)
	go func() { finished <- discoveredModels() }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery not started")
	}
	reloaded := make(chan struct{})
	go func() {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("dynamic_models: false")})
		configure(raw)
		close(reloaded)
	}()
	select {
	case <-reloaded:
	case <-time.After(3 * time.Second):
		t.Fatal("reload blocked on discovery")
	}
	// Release the upstream and ensure the invalidated generation is not cached.
	release <- struct{}{}
	select {
	case models := <-finished:
		if len(models) != 0 {
			t.Fatal("stale catalog published")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("discovery did not complete")
	}
}

func TestDiagnosticsOmitBodiesAndKeyPrefixes(t *testing.T) {
	old := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	logUpstreamBody("request", "big-pickle", 400, `Bearer PRIVATE_TOKEN prompt=PRIVATE_TEXT`, "PRIVATE_ERROR")
	writer.Close()
	os.Stderr = old
	data, _ := io.ReadAll(reader)
	reader.Close()
	if bytes.Contains(data, []byte("PRIVATE")) {
		t.Fatalf("secret in diagnostics: %s", data)
	}
	key := "sk-private-key-value"
	label := redactKey(key)
	if strings.Contains(label, key[:8]) || !strings.HasPrefix(label, "client-") || label != redactKey(key) {
		t.Fatal("client label leaks key or is unstable")
	}
}
