package executor

import (
	"errors"
	"strings"
	"testing"
)

func TestFriendlySSLError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "nil",
			err:  nil,
			want: []string{""},
		},
		{
			name: "http 404 challenge",
			err:  errors.New("Certificate retrieval failed: invalid authorization: Invalid response from http://example.com/.well-known/acme-challenge/token: 404"),
			want: []string{"HTTP-01", "A/AAAA", "CDN"},
		},
		{
			name: "dns nxdomain",
			err:  errors.New("NXDOMAIN looking up A for example.com"),
			want: []string{"DNS", "A/AAAA"},
		},
		{
			name: "connection refused",
			err:  errors.New("connect: connection refused"),
			want: []string{"80", "firewall"},
		},
		{
			name: "timeout",
			err:  errors.New("context deadline exceeded"),
			want: []string{"timed out", "80"},
		},
		{
			name: "unauthorized",
			err:  errors.New("urn:ietf:params:acme:error:unauthorized"),
			want: []string{"verification failed", "CDN"},
		},
		{
			name: "default",
			err:  errors.New("unexpected acme failure"),
			want: []string{"Let's Encrypt certificate request failed", "unexpected acme failure"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := FriendlySSLError(tt.err)
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("message = %q, want substring %q", msg, want)
				}
			}
		})
	}
}
