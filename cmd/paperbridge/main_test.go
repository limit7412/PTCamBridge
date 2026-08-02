package main

import "testing"

// A wildcard bind is a valid thing to listen on and a useless thing to write
// into the client's address cache.
func TestConnectAddress(t *testing.T) {
	cases := map[string]string{
		"[::]:18080":       "127.0.0.1:18080",
		"0.0.0.0:18080":    "127.0.0.1:18080",
		"127.0.0.1:18080":  "127.0.0.1:18080",
		"192.168.1.5:8080": "192.168.1.5:8080",
		"[::1]:18080":      "[::1]:18080",
		"not-an-address":   "not-an-address",
	}
	for addr, want := range cases {
		if got := connectAddress(addr); got != want {
			t.Errorf("connectAddress(%q) = %q, want %q", addr, got, want)
		}
	}
}
