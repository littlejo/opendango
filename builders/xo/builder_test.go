package xo

import (
	"context"
	"testing"
)

func TestLocalAddr(t *testing.T) {
	tests := []struct{ base, want string }{
		{"https://127.0.0.1", "127.0.0.1"},
		{"http://127.0.0.1:8080/", "127.0.0.1"},
		{"https://[::1]", "::1"},
	}
	for _, tt := range tests {
		b := NewBuilder(Config{}, &Client{BaseURL: tt.base}, "vm", nil)
		got, err := b.LocalAddr(context.Background())
		if err != nil || got != tt.want {
			t.Errorf("%s: LocalAddr = %q, %v; want %q", tt.base, got, err, tt.want)
		}
	}
}
