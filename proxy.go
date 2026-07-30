// proxy.go — Proxy-aware HTTP client for Qoder API.
//
// Supports: socks5://, socks5h://, http://, https:// proxy URLs.
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
	"time"

	"golang.org/x/net/proxy"
)

// proxyClient is the shared HTTP client with optional proxy support.
// Initialized in main() after .env is loaded.
var proxyClient *http.Client

// initProxyClient (re)builds the proxy-aware HTTP client from current env.
func initProxyClient() {
	proxyClient = buildProxyClient()
}

// buildProxyClient creates an HTTP Client that routes through a proxy if configured.
// Priority: QODER_PROXY > HTTPS_PROXY > ALL_PROXY
func buildProxyClient() *http.Client {
	proxyURL := firstNonEmpty(
		os.Getenv("QODER_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("https_proxy"),
		os.Getenv("ALL_PROXY"),
		os.Getenv("all_proxy"),
	)

	if proxyURL == "" {
		return &http.Client{Timeout: 5 * time.Minute}
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		log.Printf("proxy: invalid URL %q: %v — using direct", proxyURL, err)
		return &http.Client{Timeout: 5 * time.Minute}
	}

	switch u.Scheme {
	case "socks5", "socks5h":
		return buildSocks5Client(u)
	case "http", "https":
		return buildHTTPProxyClient(u)
	default:
		log.Printf("proxy: unsupported scheme %q — using direct", u.Scheme)
		return &http.Client{Timeout: 5 * time.Minute}
	}
}

func buildSocks5Client(u *url.URL) *http.Client {
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
		return &http.Client{Timeout: 5 * time.Minute}
	}

	log.Printf("proxy: SOCKS5 %s", host)
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.(contextDialer).DialContext(ctx, network, addr)
			},
		},
		Timeout: 5 * time.Minute,
	}
}

func buildHTTPProxyClient(u *url.URL) *http.Client {
	log.Printf("proxy: HTTP %s", u.Host)
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(u),
		},
		Timeout: 5 * time.Minute,
	}
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
	proxyURL := firstNonEmpty(
		os.Getenv("QODER_PROXY"),
		os.Getenv("HTTPS_PROXY"),
		os.Getenv("https_proxy"),
		os.Getenv("ALL_PROXY"),
		os.Getenv("all_proxy"),
	)
	if proxyURL == "" {
		return "none (direct)"
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Sprintf("invalid (%s)", proxyURL)
	}
	if u.User != nil {
		return fmt.Sprintf("%s://%s:***@%s", u.Scheme, u.User.Username(), u.Host)
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}
