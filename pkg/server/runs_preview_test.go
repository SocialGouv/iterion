package server

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestIsPublicUnicast(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"192.168.1.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"169.254.169.254", false},
		{"169.254.1.2", false},
		{"100.100.100.200", false}, // alibaba metadata
		{"::1", false},
		{"fe80::1", false},
		{"fd00::1", false},
		{"2001:4860:4860::8888", true},
		{"0.0.0.0", false},
		{"224.0.0.1", false}, // multicast
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) failed", tc.ip)
			}
			if got := isPublicUnicast(ip); got != tc.want {
				t.Fatalf("isPublicUnicast(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestResolvePreviewHost_NumericIP(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		cloudMode bool
		wantErr   bool
	}{
		{"public ip in cloud", "8.8.8.8", true, false},
		{"public ip in local", "8.8.8.8", false, false},
		{"loopback in cloud", "127.0.0.1", true, true},
		{"loopback in local", "127.0.0.1", false, false},
		{"private in cloud", "192.168.1.1", true, true},
		{"private in local", "192.168.1.1", false, false},
		{"metadata in cloud", "169.254.169.254", true, true},
		{"metadata in local", "169.254.169.254", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, err := resolvePreviewHost(context.Background(), tc.host, tc.cloudMode)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s in cloud=%v, got ip=%v", tc.host, tc.cloudMode, ip)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ip == nil || ip.String() != tc.host {
				t.Fatalf("got ip=%v, want %s", ip, tc.host)
			}
		})
	}
}

func TestResolvePreviewHost_ClusterInternalNames(t *testing.T) {
	rejected := []string{
		"kubernetes.default.svc.cluster.local",
		"my-service.my-ns.svc.cluster.local",
		"metadata.google.internal",
		"foo.svc",
	}
	for _, h := range rejected {
		t.Run(h, func(t *testing.T) {
			_, err := resolvePreviewHost(context.Background(), h, true)
			if err == nil {
				t.Fatalf("expected refusal of %s in cloud mode", h)
			}
			if !strings.Contains(err.Error(), "cluster-internal") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestResolvePreviewHost_EmptyHost(t *testing.T) {
	if _, err := resolvePreviewHost(context.Background(), "", false); err == nil {
		t.Fatal("expected error for empty host")
	}
	if _, err := resolvePreviewHost(context.Background(), "", true); err == nil {
		t.Fatal("expected error for empty host")
	}
}
