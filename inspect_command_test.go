package starling

import (
	"net"
	"testing"
)

// TestBrowserURL pins the wildcard-normalization contract. net.Listen
// on an unspecified host gives back addresses like "[::]:43127" or
// "0.0.0.0:43127" that no browser can open — we must rewrite the host
// portion to localhost while preserving whatever port the kernel
// chose, and we must not touch addresses that are already concrete.
func TestBrowserURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ipv6 wildcard", "[::]:43127", "http://localhost:43127"},
		{"ipv4 wildcard", "0.0.0.0:8080", "http://localhost:8080"},
		{"ipv4 loopback", "127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"named host", "myhost:9000", "http://myhost:9000"},
		{"ipv6 loopback", "[::1]:9000", "http://[::1]:9000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := browserURL(stringAddr(tc.in))
			if got != tc.want {
				t.Errorf("browserURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// stringAddr is a tiny net.Addr that just returns a fixed string —
// browserURL only consults Addr.String, so this avoids spinning up a
// real TCP listener for every case.
type stringAddr string

func (s stringAddr) Network() string { return "tcp" }
func (s stringAddr) String() string  { return string(s) }

// Compile-time check.
var _ net.Addr = stringAddr("")
