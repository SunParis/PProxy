package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPRouting(t *testing.T) {
	proxiedOrigin := textServer("proxied")
	defer proxiedOrigin.Close()
	directOrigin := textServer("direct")
	defer directOrigin.Close()

	upstream, upstreamRequests := testUpstreamProxy(t)
	defer upstream.Close()
	local := testLocalProxy(t, proxiedOrigin.URL, upstream.URL)
	defer local.Close()

	client := proxyClient(t, local.URL, false)
	assertResponse(t, client, proxiedOrigin.URL, "proxied")
	assertResponse(t, client, directOrigin.URL, "direct")
	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("upstream received %d requests, want 1", got)
	}
}

func TestCONNECTRouting(t *testing.T) {
	proxiedOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "proxied TLS")
	}))
	defer proxiedOrigin.Close()
	directOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct TLS")
	}))
	defer directOrigin.Close()

	upstream, upstreamRequests := testUpstreamProxy(t)
	defer upstream.Close()
	local := testLocalProxy(t, proxiedOrigin.URL, upstream.URL)
	defer local.Close()

	client := proxyClient(t, local.URL, true)
	assertResponse(t, client, proxiedOrigin.URL, "proxied TLS")
	assertResponse(t, client, directOrigin.URL, "direct TLS")
	if got := upstreamRequests.Load(); got != 1 {
		t.Fatalf("upstream received %d CONNECT requests, want 1", got)
	}
}

func textServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
}

func testLocalProxy(t *testing.T, listEntry, upstreamValue string) *httptest.Server {
	t.Helper()
	matcher, err := loadDestinationMatcher(strings.NewReader(listEntry + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	upstreamURL, err := url.Parse(upstreamValue)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := newForwardProxy(matcher, upstreamURL, log.New(io.Discard, "", 0), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.close)
	return httptest.NewServer(proxy)
}

func testUpstreamProxy(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	requests := &atomic.Int64{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodConnect {
			serveTestTunnel(t, w, r)
			return
		}

		outgoing := r.Clone(r.Context())
		outgoing.RequestURI = ""
		removeHopByHopHeaders(outgoing.Header)
		response, err := transport.RoundTrip(outgoing)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		copyHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(transport.CloseIdleConnections)
	return server, requests
}

func serveTestTunnel(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	backend, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		backend.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, buffer, err := hijacker.Hijack()
	if err != nil {
		backend.Close()
		t.Errorf("hijack upstream client: %v", err)
		return
	}
	_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if err := buffer.Flush(); err != nil {
		client.Close()
		backend.Close()
		return
	}
	relayConnections(&bufferedConn{Conn: client, reader: buffer.Reader}, backend)
}

func proxyClient(t *testing.T, proxyValue string, insecureTLS bool) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxyValue)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	transport.TLSClientConfig = &tls.Config{ // Test servers use ephemeral self-signed certificates.
		InsecureSkipVerify: insecureTLS,
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func assertResponse(t *testing.T, client *http.Client, destination, want string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, destination, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", destination, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("GET %s = %s %q, want 200 %q", destination, response.Status, body, want)
	}
}

func TestRequestDestination(t *testing.T) {
	request := httptest.NewRequest(http.MethodConnect, "http://example.test", nil)
	request.Host = "[2001:db8::1]:8443"
	host, port, authority, err := requestDestination(request)
	if err != nil {
		t.Fatal(err)
	}
	if host != "2001:db8::1" || port != "8443" || authority != "[2001:db8::1]:8443" {
		t.Fatal(fmt.Sprintf("unexpected destination: %q %q %q", host, port, authority))
	}
}
