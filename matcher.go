package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

type ruleKind uint8

const (
	exactHost ruleKind = iota
	domainSuffix
	ipPrefix
)

type destinationRule struct {
	kind        ruleKind
	host        string
	port        string
	includeRoot bool
	prefix      netip.Prefix
}

// destinationMatcher matches connection destinations, not URL paths.
type destinationMatcher struct {
	rules []destinationRule
}

func loadDestinationMatcher(r io.Reader) (*destinationMatcher, error) {
	scanner := bufio.NewScanner(r)
	// Permit generated lists with long lines while still bounding memory use.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	matcher := &destinationMatcher{}
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		rule, err := parseDestinationRule(line)
		if err != nil {
			return nil, fmt.Errorf("DIRECT_LIST line %d: %w", lineNumber, err)
		}
		matcher.rules = append(matcher.rules, rule)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read DIRECT_LIST: %w", err)
	}
	return matcher, nil
}

func parseDestinationRule(value string) (destinationRule, error) {
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil {
			return destinationRule{}, fmt.Errorf("invalid URL %q: %w", value, err)
		}
		if u.Hostname() == "" {
			return destinationRule{}, fmt.Errorf("URL %q has no host", value)
		}
		port := u.Port()
		if port == "" {
			port = defaultPort(u.Scheme)
		}
		if port == "" {
			return destinationRule{}, fmt.Errorf("URL %q needs an explicit port", value)
		}
		if err := validatePort(port); err != nil {
			return destinationRule{}, fmt.Errorf("URL %q: %w", value, err)
		}
		return destinationRule{kind: exactHost, host: normalizeHost(u.Hostname()), port: port}, nil
	}

	if prefix, err := netip.ParsePrefix(value); err == nil {
		return destinationRule{kind: ipPrefix, prefix: prefix.Masked()}, nil
	}

	host, port, err := splitRuleHostPort(value)
	if err != nil {
		return destinationRule{}, err
	}
	if port != "" {
		if err := validatePort(port); err != nil {
			return destinationRule{}, err
		}
	}

	includeRoot := false
	if strings.HasPrefix(host, "*.") {
		host = strings.TrimPrefix(host, "*.")
	} else if strings.HasPrefix(host, ".") {
		host = strings.TrimPrefix(host, ".")
		includeRoot = true
	} else {
		host = normalizeHost(host)
		if host == "" {
			return destinationRule{}, fmt.Errorf("invalid empty host in %q", value)
		}
		return destinationRule{kind: exactHost, host: host, port: port}, nil
	}

	host = normalizeHost(host)
	if host == "" || net.ParseIP(host) != nil {
		return destinationRule{}, fmt.Errorf("invalid domain suffix %q", value)
	}
	return destinationRule{
		kind:        domainSuffix,
		host:        host,
		port:        port,
		includeRoot: includeRoot,
	}, nil
}

func splitRuleHostPort(value string) (string, string, error) {
	if ip := net.ParseIP(strings.Trim(value, "[]")); ip != nil {
		return ip.String(), "", nil
	}

	host, port, err := net.SplitHostPort(value)
	if err == nil {
		if host == "" {
			return "", "", fmt.Errorf("invalid empty host in %q", value)
		}
		return host, port, nil
	}

	if strings.Contains(value, "/") || strings.ContainsAny(value, "?#") {
		return "", "", fmt.Errorf("invalid destination %q", value)
	}
	if strings.Count(value, ":") > 0 {
		return "", "", fmt.Errorf("invalid host or host:port %q", value)
	}
	return value, "", nil
}

func (m *destinationMatcher) Match(host, port string) bool {
	host = normalizeHost(host)
	addr, isIP := netip.ParseAddr(host)
	if isIP == nil {
		addr = addr.Unmap()
	}

	for _, rule := range m.rules {
		if rule.port != "" && rule.port != port {
			continue
		}
		switch rule.kind {
		case exactHost:
			if host == rule.host {
				return true
			}
		case domainSuffix:
			if strings.HasSuffix(host, "."+rule.host) || (rule.includeRoot && host == rule.host) {
				return true
			}
		case ipPrefix:
			if isIP == nil && rule.prefix.Contains(addr) {
				return true
			}
		}
	}
	return false
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func validatePort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http", "ws":
		return "80"
	case "https", "wss":
		return "443"
	default:
		return ""
	}
}
