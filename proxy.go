package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const connectTimeout = 10 * time.Second

type forwardProxy struct {
	matcher           *destinationMatcher
	upstream          *url.URL
	dialer            *net.Dialer
	directTransport   *http.Transport
	upstreamTransport *http.Transport
	logger            *log.Logger
	verbose           bool
}

func newForwardProxy(matcher *destinationMatcher, upstream *url.URL, logger *log.Logger, verbose bool) (*forwardProxy, error) {
	if matcher == nil {
		return nil, errors.New("destination matcher is required")
	}
	if upstream == nil || upstream.Hostname() == "" {
		return nil, errors.New("upstream proxy URL must include a host")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return nil, fmt.Errorf("unsupported upstream proxy scheme %q", upstream.Scheme)
	}
	if upstream.Path != "" && upstream.Path != "/" {
		return nil, errors.New("upstream proxy URL cannot include a path")
	}
	if upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("upstream proxy URL cannot include a query or fragment")
	}

	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	directTransport := baseTransport(dialer)
	upstreamTransport := baseTransport(dialer)
	upstreamTransport.Proxy = http.ProxyURL(upstream)

	return &forwardProxy{
		matcher:           matcher,
		upstream:          upstream,
		dialer:            dialer,
		directTransport:   directTransport,
		upstreamTransport: upstreamTransport,
		logger:            logger,
		verbose:           verbose,
	}, nil
}

func baseTransport(dialer *net.Dialer) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = dialer.DialContext
	transport.DisableCompression = true
	return transport
}

func (p *forwardProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, authority, err := requestDestination(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	useUpstream := p.matcher.Match(host, port)
	if p.verbose && p.logger != nil {
		route := "direct"
		if useUpstream {
			route = "upstream"
		}
		p.logger.Printf("%s %s via=%s", r.Method, authority, route)
	}

	if r.Method == http.MethodConnect {
		p.serveConnect(w, r, authority, useUpstream)
		return
	}
	p.serveHTTP(w, r, useUpstream)
}

func (p *forwardProxy) serveHTTP(w http.ResponseWriter, r *http.Request, useUpstream bool) {
	outgoing := r.Clone(r.Context())
	outgoing.RequestURI = ""
	if outgoing.URL.Scheme == "" {
		outgoing.URL.Scheme = "http"
	}
	if outgoing.URL.Host == "" {
		outgoing.URL.Host = r.Host
	}
	removeHopByHopHeaders(outgoing.Header)

	transport := p.directTransport
	if useUpstream {
		transport = p.upstreamTransport
	}
	response, err := transport.RoundTrip(outgoing)
	if err != nil {
		p.logError("forward request", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	removeHopByHopHeaders(response.Header)
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		p.logError("copy response", err)
	}
}

func (p *forwardProxy) serveConnect(w http.ResponseWriter, r *http.Request, authority string, useUpstream bool) {
	backend, err := p.dialTunnel(r.Context(), authority, useUpstream)
	if err != nil {
		p.logError("open tunnel", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		backend.Close()
		http.Error(w, "connection hijacking is not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuffer, err := hijacker.Hijack()
	if err != nil {
		backend.Close()
		p.logError("hijack client connection", err)
		return
	}

	if _, err := clientBuffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		clientConn.Close()
		backend.Close()
		return
	}
	if err := clientBuffer.Flush(); err != nil {
		clientConn.Close()
		backend.Close()
		return
	}

	client := &bufferedConn{Conn: clientConn, reader: clientBuffer.Reader}
	relayConnections(client, backend)
}

func (p *forwardProxy) dialTunnel(ctx context.Context, authority string, useUpstream bool) (net.Conn, error) {
	if !useUpstream {
		return p.dialer.DialContext(ctx, "tcp", authority)
	}

	proxyAddress := p.upstream.Host
	if p.upstream.Port() == "" {
		proxyAddress = net.JoinHostPort(p.upstream.Hostname(), defaultPort(p.upstream.Scheme))
	}
	conn, err := p.dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("dial upstream proxy: %w", err)
	}

	if p.upstream.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: p.upstream.Hostname(),
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("TLS handshake with upstream proxy: %w", err)
		}
		conn = tlsConn
	}

	connectRequest := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: authority},
		Host:   authority,
		Header: make(http.Header),
	}
	if p.upstream.User != nil {
		password, _ := p.upstream.User.Password()
		credentials := p.upstream.User.Username() + ":" + password
		connectRequest.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	if err := connectRequest.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT to upstream proxy: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, connectRequest)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read upstream CONNECT response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("upstream CONNECT returned %s", response.Status)
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func requestDestination(r *http.Request) (host, port, authority string, err error) {
	if r.Method == http.MethodConnect {
		authority = r.Host
		if authority == "" {
			authority = r.URL.Host
		}
		host, port, err = splitAuthority(authority, "443")
	} else {
		host = r.URL.Hostname()
		port = r.URL.Port()
		if host == "" {
			host, port, err = splitAuthority(r.Host, defaultPort(r.URL.Scheme))
		} else if port == "" {
			port = defaultPort(r.URL.Scheme)
			if port == "" {
				port = "80"
			}
		}
	}
	if err != nil {
		return "", "", "", fmt.Errorf("invalid destination: %w", err)
	}
	if host == "" {
		return "", "", "", errors.New("invalid destination: missing host")
	}
	if err := validatePort(port); err != nil {
		return "", "", "", fmt.Errorf("invalid destination: %w", err)
	}
	host = normalizeHost(host)
	authority = net.JoinHostPort(host, port)
	return host, port, authority, nil
}

func splitAuthority(authority, fallbackPort string) (string, string, error) {
	if authority == "" {
		return "", "", errors.New("missing authority")
	}
	if host, port, err := net.SplitHostPort(authority); err == nil {
		return host, port, nil
	}
	if ip := net.ParseIP(strings.Trim(authority, "[]")); ip != nil && fallbackPort != "" {
		return ip.String(), fallbackPort, nil
	}
	if !strings.Contains(authority, ":") && fallbackPort != "" {
		return authority, fallbackPort, nil
	}
	return "", "", fmt.Errorf("invalid authority %q", authority)
}

func removeHopByHopHeaders(header http.Header) {
	for _, connectionValue := range header.Values("Connection") {
		for _, name := range strings.Split(connectionValue, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(name)
	}
}

func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func (p *forwardProxy) close() {
	p.directTransport.CloseIdleConnections()
	p.upstreamTransport.CloseIdleConnections()
}

func (p *forwardProxy) logError(operation string, err error) {
	if p.logger != nil {
		p.logger.Printf("%s: %v", operation, err)
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}

func (c *bufferedConn) CloseWrite() error {
	if connection, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return connection.CloseWrite()
	}
	return nil
}

func relayConnections(left, right net.Conn) {
	defer left.Close()
	defer right.Close()

	done := make(chan struct{}, 2)
	copyOneWay := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if connection, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = connection.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyOneWay(left, right)
	go copyOneWay(right, left)
	<-done
	<-done
}
