package main

import "testing"

func TestInviteURL(t *testing.T) {
	for _, tc := range []struct{ in, want, err string }{
		{"https://share.example.com/i/abc.md", "https://share.example.com/i/abc", ""},
		{"http://127.0.0.1:8742/i/abc", "http://127.0.0.1:8742/i/abc", ""},
		{"http://[::1]:8742/i/abc", "http://[::1]:8742/i/abc", ""},
		{"http://localhost/i/abc", "http://localhost/i/abc", ""},
		{"http://example.com/i/x", "", `"http://example.com/i/x" is plaintext http, and the answer carries the key this machine will trust; use the https:// invite URL`},
		{"http://127.0.0.1.example.com/i/x", "", `"http://127.0.0.1.example.com/i/x" is plaintext http, and the answer carries the key this machine will trust; use the https:// invite URL`},
		{"ftp://share.example.com/i/abc", "", `"ftp://share.example.com/i/abc" is not an invite URL (https://host/i/<token>)`},
		{"https://share.example.com/s/abc", "", `"https://share.example.com/s/abc" is not an invite URL (https://host/i/<token>)`},
	} {
		got, err := inviteURL(tc.in)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if got != tc.want || msg != tc.err {
			t.Errorf("inviteURL(%q) = %q, %q; want %q, %q", tc.in, got, msg, tc.want, tc.err)
		}
	}
}
