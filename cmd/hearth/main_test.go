package main

import (
	"strings"
	"testing"

	"github.com/soneshsk/hearth/internal/config"
)

func TestValidateServerBind(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		wantErr    bool
		errMessage string
	}{
		{name: "loopback ipv4", host: "127.0.0.1"},
		{name: "loopback ipv4 second", host: "127.0.0.2"},
		{name: "loopback hostname", host: "localhost"},
		{name: "loopback hostname case", host: "LOCALHOST"},
		{name: "loopback ipv6", host: "::1"},
		{name: "loopback ipv6 bracketed", host: "[::1]"},
		{name: "loopback mapped", host: "::ffff:127.0.0.1"},
		{name: "all interfaces v4", host: "0.0.0.0", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "all interfaces v6", host: "::", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "lan address", host: "192.168.1.20", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "lan address 10", host: "10.0.0.5", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "public address", host: "8.8.8.8", wantErr: true, errMessage: "plaintext HTTP only"},
		{name: "public ipv6", host: "2001:db8::1", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "empty host", host: "", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "malformed host", host: "not a host!!", wantErr: true, errMessage: "refusing non-loopback bind"},
		{name: "hostname non-loopback", host: "example.com", wantErr: true, errMessage: "refusing non-loopback bind"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServerBind(config.Config{Host: tt.host})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected non-loopback bind to be rejected")
				}
				if !strings.Contains(err.Error(), tt.errMessage) {
					t.Fatalf("expected error to contain %q, got %q", tt.errMessage, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected loopback bind to be accepted, got %v", err)
			}
		})
	}
}

func TestValidateAuthConfig(t *testing.T) {
	if err := validateAuthConfig(config.Config{APIKey: "public"}, 1); err == nil {
		t.Fatal("expected \"public\" admin key to be rejected")
	}
	if err := validateAuthConfig(config.Config{APIKey: "short"}, 1); err == nil {
		t.Fatal("expected short admin key to be rejected")
	}
	if err := validateAuthConfig(config.Config{APIKey: "0123456789abcdef"}, 1); err != nil {
		t.Fatalf("expected strong admin key to be accepted, got %v", err)
	}
	// Empty admin key is allowed: per-account tokens still protect chat,
	// admin endpoints fail closed at request time.
	if err := validateAuthConfig(config.Config{}, 1); err != nil {
		t.Fatalf("expected empty admin key to be allowed with warning, got %v", err)
	}
}

func TestIsLoopbackHostVectors(t *testing.T) {
	allowed := []string{"127.0.0.1", "127.0.0.2", "localhost", "LOCALHOST", "::1", "[::1]", "::ffff:127.0.0.1"}
	for _, h := range allowed {
		if !config.IsLoopbackHost(h) {
			t.Errorf("expected %q to be loopback", h)
		}
	}
	denied := []string{"0.0.0.0", "::", "192.168.1.20", "10.0.0.5", "8.8.8.8", "", "not a host!!", "example.com", "2001:db8::1"}
	for _, h := range denied {
		if config.IsLoopbackHost(h) {
			t.Errorf("expected %q to be rejected", h)
		}
	}
}
