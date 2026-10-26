package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Aligned with App.useragent: opencode/<channel>/<version>/<client>.
const upstreamUserAgent = "opencode/latest/2.0.29/cli"

// The free tier does not require a real token.
const upstreamAuthHeader = "Bearer public"

var (
	httpClientsMu sync.Mutex
	proxyClients  = map[string]*http.Client{}
)

// Clients retain normal TLS verification and honor explicit proxies without
// silently falling back to a different route. The total deadline also covers
// response bodies; redirects must not send a conversation to another endpoint.
func sharedHTTPClient() (*http.Client, error) {
	return clientForAuth("")
}

func clientForAuth(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		proxyURL = readHostProxy()
	}
	cfg := readSettings()
	key := fmt.Sprintf("%s|%d|%d", proxyURL, cfg.ConnectTimeoutMS, cfg.RequestTimeoutMS)
	httpClientsMu.Lock()
	defer httpClientsMu.Unlock()
	if client, ok := proxyClients[key]; ok {
		return client, nil
	}
	client, err := buildClient(proxyURL, cfg.ConnectTimeoutMS)
	if err != nil {
		return nil, err
	}
	deadline := cfg.RequestTimeoutMS
	if deadline <= 0 {
		deadline = 600_000
	}
	client.Timeout = time.Duration(deadline) * time.Millisecond
	if len(proxyClients) >= 32 {
		for k, old := range proxyClients {
			old.CloseIdleConnections()
			delete(proxyClients, k)
		}
	}
	proxyClients[key] = client
	return client, nil
}

func buildClient(proxyURL string, connectTimeoutMS int) (*http.Client, error) {
	timeout := time.Duration(clampConnectTimeout(connectTimeoutMS)) * time.Millisecond
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	transport.TLSHandshakeTimeout = timeout
	transport.DialContext = (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" ||
			parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return nil, fmt.Errorf("invalid configured proxy URL")
		}
		switch parsed.Scheme {
		case "http", "https", "socks5", "socks5h":
			transport.Proxy = http.ProxyURL(parsed)
		default:
			return nil, fmt.Errorf("unsupported configured proxy scheme")
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("upstream redirects are disabled")
		},
	}, nil
}

// upstreamHeaders mirrors zen.fetchUpstream: the upstream merges sessions on
// x-opencode-session and validates the OpenCode client identity headers.
func upstreamHeaders(sessionID string) map[string]string {
	return map[string]string{
		"Accept":                "text/event-stream",
		"Content-Type":          "application/json",
		"Authorization":         upstreamAuthHeader,
		"User-Agent":            upstreamUserAgent,
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
		"x-opencode-session":    sessionID,
		"x-opencode-session-id": sessionID,
		"x-session-affinity":    sessionID,
		"X-Session-Id":          sessionID,
	}
}

func chatURL() string {
	return readSettings().UpstreamBaseURL + "/chat/completions"
}

// newUpstreamChatRequest builds the streaming chat request.
func newUpstreamChatRequest(ctx context.Context, built *upstreamRequest) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(), bytes.NewReader(built.Body))
	if err != nil {
		return nil, err
	}
	for key, value := range upstreamHeaders(built.SessionID) {
		request.Header.Set(key, value)
	}
	return request, nil
}

// newUpstreamModelsRequest builds the model discovery request.
func newUpstreamModelsRequest(modelsURL string) (*http.Request, error) {
	request, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", upstreamAuthHeader)
	request.Header.Set("User-Agent", upstreamUserAgent)
	return request, nil
}

func fsDebugLog(message string) {
	// The c-shared plugin shares the host process, so this writes to the
	// host's stderr — the same stream CLIProxyAPI's console logger uses.
	fmt.Fprintln(os.Stderr, "[oc2cpa] "+message)
}
