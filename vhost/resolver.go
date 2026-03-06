package vhost

import (
	"net"
	"strings"
	"sync"
	"time"
)

// DNSLookup is a function that looks up TXT records for a host.
// Injected for testability (defaults to net.LookupTXT).
type DNSLookup func(host string) ([]string, error)

// Resolver maps Host headers to PAGE refs using TXT records and hash subdomains.
type Resolver struct {
	baseDomain string
	cacheTTL   time.Duration
	lookupTXT  DNSLookup

	mu       sync.RWMutex
	txtCache map[string]txtEntry
	hashMap  map[string]string // hash -> ref
}

type txtEntry struct {
	ref       string
	expiresAt time.Time
}

// NewResolver creates a Resolver with the given base domain and cache TTL.
// If lookup is nil, net.LookupTXT is used.
func NewResolver(baseDomain string, cacheTTL time.Duration, lookup DNSLookup) *Resolver {
	if lookup == nil {
		lookup = net.LookupTXT
	}
	return &Resolver{
		baseDomain: baseDomain,
		cacheTTL:   cacheTTL,
		lookupTXT:  lookup,
		txtCache:   make(map[string]txtEntry),
		hashMap:    make(map[string]string),
	}
}

// BaseDomain returns the configured base domain.
func (r *Resolver) BaseDomain() string {
	return r.baseDomain
}

// Resolve returns the PAGE ref for a Host header value, or "".
// Resolution order: bare domain → custom domain TXT → subdomain TXT → hash lookup.
func (r *Resolver) Resolve(host string) string {
	host = stripPort(host)

	sub := r.Subdomain(host)
	if sub == "" {
		if host == r.baseDomain {
			// Bare base domain — no PAGE
			return ""
		}
		// Custom domain (e.g. dataverse.social) — try TXT lookup
		return r.lookupCachedTXT(host)
	}

	// TXT lookup (cached)
	if ref := r.lookupCachedTXT(host); ref != "" {
		return ref
	}

	// Hash map lookup
	r.mu.RLock()
	ref := r.hashMap[sub]
	r.mu.RUnlock()
	return ref
}

// Subdomain extracts the subdomain prefix from a host, given the base domain.
// Returns "" if host IS the base domain or doesn't end with it.
func (r *Resolver) Subdomain(host string) string {
	host = stripPort(host)

	if host == r.baseDomain {
		return ""
	}
	suffix := "." + r.baseDomain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// IsPageHost checks if the host matches a given PAGE ref
// (either via TXT record or hash subdomain).
func (r *Resolver) IsPageHost(host, pageRef string) bool {
	resolved := r.Resolve(host)
	if resolved == pageRef {
		return true
	}
	// Also check hash match directly (avoids TXT lookup for redirects)
	sub := r.Subdomain(host)
	return sub == PageHash(pageRef)
}

// UpdateHashMap replaces the hash→ref map. Called on startup and when PAGEs change.
func (r *Resolver) UpdateHashMap(pages map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hashMap = pages
}

// AddPage adds a single PAGE to the hash map.
func (r *Resolver) AddPage(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hashMap[PageHash(ref)] = ref
}

// RemovePage removes a PAGE from the hash map.
func (r *Resolver) RemovePage(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hashMap, PageHash(ref))
}

// lookupCachedTXT returns the PAGE ref from DNS TXT records, using a cache.
func (r *Resolver) lookupCachedTXT(host string) string {
	now := time.Now()

	r.mu.RLock()
	if e, ok := r.txtCache[host]; ok && now.Before(e.expiresAt) {
		r.mu.RUnlock()
		return e.ref
	}
	r.mu.RUnlock()

	// DNS lookup at _dv.{host} (outside lock)
	records, err := r.lookupTXT("_dv." + host)
	ref := ""
	if err == nil {
		for _, txt := range records {
			if strings.HasPrefix(txt, "dv1-page=") {
				ref = strings.TrimPrefix(txt, "dv1-page=")
				break
			}
			// Also accept bare ref (no prefix)
			if strings.Contains(txt, ".") && !strings.Contains(txt, "=") {
				ref = txt
				break
			}
		}
	}

	// Cache result (positive or negative)
	r.mu.Lock()
	r.txtCache[host] = txtEntry{ref: ref, expiresAt: now.Add(r.cacheTTL)}
	r.mu.Unlock()

	return ref
}

// stripPort removes the port from a host:port string.
func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i != -1 {
		// Check if this is an IPv6 address
		if strings.Contains(host, "]") {
			// IPv6 with bracket notation [::1]:8080
			if bi := strings.LastIndex(host, "]"); bi < i {
				return host[:i]
			}
			return host
		}
		return host[:i]
	}
	return host
}
