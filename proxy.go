// proxy.go — Proxy-aware HTTP client for Qoder API.
//
// Supports: socks5://, socks5h://, http://, https:// proxy URLs.
// Multi-proxy: comma-separated list rotates per request.
// Reads from QODER_PROXY env, then HTTPS_PROXY, then ALL_PROXY.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// proxyPool holds multiple HTTP clients for proxy rotation.
var proxyPool []*http.Client

// proxyLabels holds human-readable descriptions for logging.
var proxyLabels []string

// proxyIdx is the round-robin counter for proxy rotation.
var proxyIdx int
var proxyMu sync.Mutex

// streamingTransport returns a transport suitable for SSE streaming (no overall timeout).
// Optimized for low latency: DNS cache, connection reuse, no compression.
func streamingTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     120 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
		// ponytail: no ReadTimeout here — SSE streams can run indefinitely.
		// Context cancellation (client disconnect) handles the upper bound.
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
}

// initProxyClient builds the proxy-aware HTTP clients from current env.
func initProxyClient() {
	proxyURL := firstNonEmpty(
		os.Getenv("QODER_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("https_proxy"),
		os.Getenv("ALL_PROXY"),
		os.Getenv("all_proxy"),
	)

	proxyMu.Lock()
	defer proxyMu.Unlock()
	for _, client := range proxyPool {
		if t, ok := client.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
	proxyPool = nil
	proxyLabels = nil
	proxyIdx = 0

	if proxyURL == "" {
		proxyPool = []*http.Client{{Transport: streamingTransport()}}
		proxyLabels = []string{"direct"}
		return
	}

	// Split comma-separated proxies
	entries := strings.Split(proxyURL, ",")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		client, label := buildSingleProxyClient(entry)
		proxyPool = append(proxyPool, client)
		proxyLabels = append(proxyLabels, label)
	}

	if len(proxyPool) == 0 {
		proxyPool = []*http.Client{{Transport: streamingTransport()}}
		proxyLabels = []string{"direct"}
	}
}

// proxyClientFn returns a round-robin proxy client from the pool.
func proxyClientFn() *http.Client {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if len(proxyPool) == 0 {
		return &http.Client{Transport: streamingTransport()}
	}
	client := proxyPool[proxyIdx%len(proxyPool)]
	proxyIdx++
	return client
}

func proxyCount() int {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if len(proxyPool) == 0 {
		return 1
	}
	return len(proxyPool)
}

func buildSingleProxyClient(proxyURL string) (*http.Client, string) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("proxy: invalid URL %q: %v — using direct", proxyURL, err)
		return &http.Client{Transport: streamingTransport()}, "direct"
	}

	switch u.Scheme {
	case "socks5", "socks5h":
		return buildSocks5Client(u)
	case "http", "https":
		return buildHTTPProxyClient(u)
	default:
		log.Printf("proxy: unsupported scheme %q — using direct", u.Scheme)
		return &http.Client{Transport: streamingTransport()}, "direct"
	}
}

func buildSocks5Client(u *url.URL) (*http.Client, string) {
	auth := &proxy.Auth{}
	if u.User != nil {
		auth.User = u.User.Username()
		if p, ok := u.User.Password(); ok {
			auth.Password = p
		}
		if auth.User == "" {
			auth = nil
		}
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":1080"
	}

	dialer, err := proxy.SOCKS5("tcp", host, auth, proxy.Direct)
	if err != nil {
		log.Printf("proxy: socks5 dialer error: %v — using direct", err)
		return &http.Client{Transport: streamingTransport()}, "direct"
	}

	label := fmt.Sprintf("socks5://%s", host)
	if u.User != nil {
		label = fmt.Sprintf("socks5://%s:***@%s", u.User.Username(), host)
	}

	t := streamingTransport()
	// Honor ctx cancellation even when the SOCKS5 dialer doesn't
	// expose DialContext. proxy.Direct has no timeout and a stalled
	// handshake can otherwise pin a goroutine past client disconnect.
	cd, _ := dialer.(contextDialer)
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if cd != nil {
			return cd.DialContext(ctx, network, addr)
		}
		type dialResult struct {
			c   net.Conn
			err error
		}
		ch := make(chan dialResult, 1)
		go func() {
			c, err := dialer.Dial(network, addr)
			ch <- dialResult{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.c, r.err
		}
	}
	return &http.Client{Transport: t}, label
}

func buildHTTPProxyClient(u *url.URL) (*http.Client, string) {
	label := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	if u.User != nil {
		label = fmt.Sprintf("%s://%s:***@%s", u.Scheme, u.User.Username(), u.Host)
	}
	t := streamingTransport()
	t.Proxy = http.ProxyURL(u)
	return &http.Client{Transport: t}, label
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// contextDialer is the interface that golang.org/x/net/proxy.SOCKS5 dialers implement.
type contextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// getProxyInfo returns a human-readable proxy description for logging.
func getProxyInfo() string {
	if len(proxyLabels) == 0 {
		return "none (direct)"
	}
	if len(proxyLabels) == 1 {
		return proxyLabels[0]
	}
	return fmt.Sprintf("%d proxies: %s", len(proxyLabels), strings.Join(proxyLabels, ", "))
}
