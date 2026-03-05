package vhost

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func mockDNS(records map[string][]string) DNSLookup {
	return func(host string) ([]string, error) {
		if r, ok := records[host]; ok {
			return r, nil
		}
		return nil, fmt.Errorf("no such host")
	}
}

func TestSubdomain(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	tests := []struct {
		host string
		want string
	}{
		{"social.example.com", "social"},
		{"example.com", ""},
		{"other.net", ""},
		{"auth.example.com", "auth"},
		{"a3f7c2e1b4d5f6a8.example.com", "a3f7c2e1b4d5f6a8"},
		{"deep.sub.example.com", "deep.sub"},
		{"social.example.com:8080", "social"},
		{"example.com:8080", ""},
	}

	for _, tt := range tests {
		got := r.Subdomain(tt.host)
		if got != tt.want {
			t.Errorf("Subdomain(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestResolve_TXTRecord(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(map[string][]string{
		"_dv.social.example.com": {"dv1-page=pk.uuid-social"},
	}))

	got := r.Resolve("social.example.com")
	if got != "pk.uuid-social" {
		t.Errorf("Resolve(social) = %q, want %q", got, "pk.uuid-social")
	}
}

func TestResolve_TXTBareRef(t *testing.T) {
	// Accept bare ref (no dv1-page= prefix)
	r := NewResolver("example.com", 5*time.Minute, mockDNS(map[string][]string{
		"_dv.social.example.com": {"pk.uuid-social"},
	}))

	got := r.Resolve("social.example.com")
	if got != "pk.uuid-social" {
		t.Errorf("Resolve(bare ref) = %q, want %q", got, "pk.uuid-social")
	}
}

func TestResolve_BareDomain(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	got := r.Resolve("example.com")
	if got != "" {
		t.Errorf("Resolve(bare domain) = %q, want empty", got)
	}
}

func TestResolve_AuthSubdomain(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	got := r.Resolve("auth.example.com")
	if got != WidgetSentinel {
		t.Errorf("Resolve(auth) = %q, want %q", got, WidgetSentinel)
	}
}

func TestResolve_AuthTXTOverride(t *testing.T) {
	// If _dv.auth.example.com has a TXT record, TXT wins over the auth sentinel
	r := NewResolver("example.com", 5*time.Minute, mockDNS(map[string][]string{
		"_dv.auth.example.com": {"dv1-page=pk.uuid-custom-auth"},
	}))

	got := r.Resolve("auth.example.com")
	if got != "pk.uuid-custom-auth" {
		t.Errorf("Resolve(auth with TXT) = %q, want %q", got, "pk.uuid-custom-auth")
	}
}

func TestResolve_HashMap(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	ref := "pk.uuid-page1"
	hash := PageHash(ref)
	r.UpdateHashMap(map[string]string{hash: ref})

	got := r.Resolve(hash + ".example.com")
	if got != ref {
		t.Errorf("Resolve(hash) = %q, want %q", got, ref)
	}
}

func TestResolve_UnknownHost(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	got := r.Resolve("unknown.example.com")
	if got != "" {
		t.Errorf("Resolve(unknown) = %q, want empty", got)
	}
}

func TestResolve_CustomDomain(t *testing.T) {
	// Custom domain not a subdomain of base — TXT lookup at _dv.{domain}
	r := NewResolver("example.com", 5*time.Minute, mockDNS(map[string][]string{
		"_dv.dataverse.social": {"pk.uuid-social-page"},
	}))

	got := r.Resolve("dataverse.social")
	if got != "pk.uuid-social-page" {
		t.Errorf("Resolve(custom domain) = %q, want %q", got, "pk.uuid-social-page")
	}
}

func TestResolve_CustomDomainNoTXT(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	got := r.Resolve("random.org")
	if got != "" {
		t.Errorf("Resolve(custom domain no TXT) = %q, want empty", got)
	}
}

func TestResolve_TXTWinsOverHash(t *testing.T) {
	ref := "pk.uuid-page1"
	hash := PageHash(ref)

	r := NewResolver("example.com", 5*time.Minute, mockDNS(map[string][]string{
		"_dv." + hash + ".example.com": {"dv1-page=pk.uuid-different"},
	}))
	r.UpdateHashMap(map[string]string{hash: ref})

	got := r.Resolve(hash + ".example.com")
	if got != "pk.uuid-different" {
		t.Errorf("Resolve(TXT vs hash) = %q, want %q", got, "pk.uuid-different")
	}
}

func TestTXTCaching(t *testing.T) {
	var lookupCount atomic.Int32

	lookup := func(host string) ([]string, error) {
		lookupCount.Add(1)
		if host == "_dv.social.example.com" {
			return []string{"dv1-page=pk.uuid-social"}, nil
		}
		return nil, fmt.Errorf("no such host")
	}

	r := NewResolver("example.com", 100*time.Millisecond, lookup)

	// First call triggers DNS lookup
	r.Resolve("social.example.com")
	if lookupCount.Load() != 1 {
		t.Fatalf("expected 1 lookup, got %d", lookupCount.Load())
	}

	// Second call uses cache
	r.Resolve("social.example.com")
	if lookupCount.Load() != 1 {
		t.Fatalf("expected 1 lookup (cached), got %d", lookupCount.Load())
	}

	// Wait for TTL to expire
	time.Sleep(150 * time.Millisecond)

	// Third call triggers fresh lookup
	r.Resolve("social.example.com")
	if lookupCount.Load() != 2 {
		t.Fatalf("expected 2 lookups (TTL expired), got %d", lookupCount.Load())
	}
}

func TestNegativeTXTCaching(t *testing.T) {
	var lookupCount atomic.Int32

	lookup := func(host string) ([]string, error) {
		lookupCount.Add(1)
		return nil, fmt.Errorf("no such host")
	}

	r := NewResolver("example.com", 100*time.Millisecond, lookup)

	// First call triggers DNS lookup (miss)
	r.Resolve("missing.example.com")
	if lookupCount.Load() != 1 {
		t.Fatalf("expected 1 lookup, got %d", lookupCount.Load())
	}

	// Second call uses negative cache
	r.Resolve("missing.example.com")
	if lookupCount.Load() != 1 {
		t.Fatalf("expected 1 lookup (negative cached), got %d", lookupCount.Load())
	}
}

func TestIsPageHost(t *testing.T) {
	ref := "pk.uuid-page1"
	hash := PageHash(ref)

	r := NewResolver("example.com", 5*time.Minute, mockDNS(map[string][]string{
		"_dv.social.example.com": {"dv1-page=" + ref},
	}))
	r.UpdateHashMap(map[string]string{hash: ref})

	tests := []struct {
		host    string
		pageRef string
		want    bool
	}{
		{"social.example.com", ref, true},          // TXT match
		{hash + ".example.com", ref, true},          // hash match
		{"other.example.com", ref, false},           // no match
		{"example.com", ref, false},                 // bare domain
		{"social.example.com", "pk.other-ref", false}, // TXT points elsewhere
	}

	for _, tt := range tests {
		got := r.IsPageHost(tt.host, tt.pageRef)
		if got != tt.want {
			t.Errorf("IsPageHost(%q, %q) = %v, want %v", tt.host, tt.pageRef, got, tt.want)
		}
	}
}

func TestAddPage(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	ref := "pk.uuid-new-page"
	r.AddPage(ref)

	hash := PageHash(ref)
	got := r.Resolve(hash + ".example.com")
	if got != ref {
		t.Errorf("after AddPage, Resolve = %q, want %q", got, ref)
	}
}

func TestRemovePage(t *testing.T) {
	r := NewResolver("example.com", 5*time.Minute, mockDNS(nil))

	ref := "pk.uuid-page"
	r.AddPage(ref)
	r.RemovePage(ref)

	hash := PageHash(ref)
	got := r.Resolve(hash + ".example.com")
	if got != "" {
		t.Errorf("after RemovePage, Resolve = %q, want empty", got)
	}
}

func TestStripPort(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"example.com", "example.com"},
		{"example.com:8080", "example.com"},
		{"localhost:5678", "localhost"},
		{"localhost", "localhost"},
	}
	for _, tt := range tests {
		got := stripPort(tt.input)
		if got != tt.want {
			t.Errorf("stripPort(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolve_WithPort(t *testing.T) {
	r := NewResolver("localhost", 5*time.Minute, mockDNS(nil))

	ref := "pk.uuid-page"
	r.AddPage(ref)
	hash := PageHash(ref)

	got := r.Resolve(hash + ".localhost:5678")
	if got != ref {
		t.Errorf("Resolve with port = %q, want %q", got, ref)
	}
}
