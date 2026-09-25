package feed

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsPublic(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"93.184.216.34", true},
		{"8.8.8.8", true},
		{"2606:4700::1111", true},
		{"100.63.255.255", true},
		{"100.128.0.1", true},
		{"127.0.0.1", false},
		{"127.1.2.3", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.10", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.100.100.200", false}, // Alibaba Cloud metadata, carrier-grade NAT space
		{"0.0.0.0", false},
		{"224.0.0.1", false},
		{"::1", false},
		{"::", false},
		{"fe80::1", false},
		{"fd00:ec2::254", false}, // AWS IPv6 metadata
		{"ff02::1", false},
	}
	for _, tt := range tests {
		if got := isPublic(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("isPublic(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestRefusePrivate(t *testing.T) {
	tests := []struct {
		address string
		refused bool
	}{
		{"93.184.216.34:443", false},
		{"[2606:4700::1111]:443", false},
		{"127.0.0.1:80", true},
		{"[::ffff:127.0.0.1]:80", true}, // IPv4-mapped
		{"[::ffff:169.254.169.254]:80", true},
		{"169.254.169.254:80", true},
		{"[fe80::1%25en0]:80", true},
		{"not-an-address", true},
	}
	for _, tt := range tests {
		err := refusePrivate("tcp", tt.address, nil)
		if refused := errors.Is(err, ErrPrivateAddress); refused != tt.refused {
			t.Errorf("refusePrivate(%q) = %v, want refused %v", tt.address, err, tt.refused)
		}
	}
}

func TestClientRefusesPrivateAddresses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(readFixture(t, "rss2.xml"))
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndexByte(srv.URL, ':'):]

	c := NewClient(Options{HostSpacing: -1})
	for _, u := range []string{srv.URL + "/feed", "http://localhost" + port + "/feed"} {
		_, err := c.Fetch(t.Context(), u, "", "")
		if !errors.Is(err, ErrPrivateAddress) || !strings.Contains(err.Error(), "is a private network address") {
			t.Errorf("Fetch(%s) = %v, want a refused private address", u, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the private server got %d requests", n)
	}

	allowed := NewClient(Options{HostSpacing: -1, AllowPrivateNetworks: true})
	if _, err := allowed.Fetch(t.Context(), srv.URL+"/feed", "", ""); err != nil {
		t.Errorf("Fetch with AllowPrivateNetworks = %v", err)
	}
}
