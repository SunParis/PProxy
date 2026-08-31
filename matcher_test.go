package main

import (
	"strings"
	"testing"
)

func TestDestinationMatcher(t *testing.T) {
	list := `
# URLs are reduced to scheme, host, and port.
http://192.168.12.23:3000/a/path
example.com
*.internal.example:8443
.service.example
10.20.0.0/16
https://secure.example/login
`
	matcher, err := loadDestinationMatcher(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		host string
		port string
		want bool
	}{
		{"URL exact destination", "192.168.12.23", "3000", true},
		{"URL port differs", "192.168.12.23", "80", false},
		{"bare host any port", "EXAMPLE.COM.", "8080", true},
		{"wildcard subdomain", "api.internal.example", "8443", true},
		{"wildcard excludes root", "internal.example", "8443", false},
		{"wildcard port differs", "api.internal.example", "443", false},
		{"dot suffix includes root", "service.example", "80", true},
		{"dot suffix includes child", "api.service.example", "80", true},
		{"CIDR contains literal IP", "10.20.5.9", "22", true},
		{"CIDR excludes other IP", "10.21.5.9", "22", false},
		{"URL gets default HTTPS port", "secure.example", "443", true},
		{"URL default port differs", "secure.example", "80", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matcher.Match(test.host, test.port); got != test.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", test.host, test.port, got, test.want)
			}
		})
	}
}

func TestDestinationMatcherReportsLineNumber(t *testing.T) {
	_, err := loadDestinationMatcher(strings.NewReader("example.com\nnot/a/host\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("expected line-numbered error, got %v", err)
	}
}
