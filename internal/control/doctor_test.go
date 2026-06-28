package control

import (
	"strings"
	"testing"
)

func TestRedactMCPError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // patterns the output must NOT contain
	}{
		{
			name: "auth header",
			in:   "connection failed: 401 Unauthorized, Authorization: Bearer sk-1234567890abcdef",
			want: []string{"sk-1234567890abcdef", "Bearer sk-"},
		},
		{
			name: "token param",
			in:   "GET /api?token=secret123 returned 403",
			want: []string{"token=secret123"},
		},
		{
			name: "password param",
			in:   "auth error: password=MyP@ssw0rd invalid",
			want: []string{"password=MyP@ssw0rd", "MyP@ssw0rd"},
		},
		{
			name: "api_key",
			in:   "failed with api_key=AKIAIOSFODNN7EXAMPLE",
			want: []string{"api_key=AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		},
		{
			name: "long message truncate",
			in:   strings.Repeat("a", 200),
			want: []string{strings.Repeat("a", 151)}, // should be cut at 150 + "..."
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactMCPError(tc.in)
			for _, forbidden := range tc.want {
				if strings.Contains(out, forbidden) {
					t.Errorf("redacted output still contains %q:\n  in:  %s\n  out: %s", forbidden, tc.in, out)
				}
			}
			if len(tc.in) > 150 && len(out) > 154 {
				t.Errorf("long message not truncated: len(out)=%d, want ≤153 (150 + '...')", len(out))
			}
		})
	}
}
