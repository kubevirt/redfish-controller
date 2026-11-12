package logger

import "testing"

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"query string marked", "https://images.example.com/images/abc/full.iso?api_key=SECRET&arch=x86_64", "https://images.example.com/images/abc/full.iso?<redacted>"},
		{"username and password marked", "https://user:SECRET@host.example.com/boot.iso", "https://<redacted>@host.example.com/boot.iso"},
		{"username alone marked", "https://SECRET@host.example.com/boot.iso", "https://<redacted>@host.example.com/boot.iso"},
		{"fragment marked", "http://host/boot.iso#SECRET", "http://host/boot.iso#<redacted>"},
		{"every removed piece marked", "https://user:SECRET@host:8443/boot.iso?token=SECRET#SECRET", "https://<redacted>@host:8443/boot.iso?<redacted>#<redacted>"},
		{"port kept", "http://slow-iso.ns.svc:8080/alpine.iso", "http://slow-iso.ns.svc:8080/alpine.iso"},
		{"plain url unchanged", "http://example.com/image.iso", "http://example.com/image.iso"},
		{"unparsable", "://not-a-url?token=SECRET", "<unparsable url>"},
		{"empty", "", "<unparsable url>"},
		{"no scheme or host", "SECRET", "<unparsable url>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.in); got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
