package serving

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tijszwinkels/dataverse-hub/vhost"
)

const (
	VhostModeOff      = "off"
	VhostModeRedirect = "redirect"
	VhostModeIsolate  = "isolate"
)

func normalizeVhostMode(mode string) string {
	switch mode {
	case "", VhostModeIsolate:
		return VhostModeIsolate
	case VhostModeRedirect:
		return VhostModeRedirect
	case VhostModeOff:
		return VhostModeOff
	default:
		return VhostModeIsolate
	}
}

func baseHostMatches(host, baseDomain string) bool {
	host = stripHostPort(host)
	return host == baseDomain
}

func stripHostPort(host string) string {
	if i := strings.LastIndex(host, ":"); i != -1 {
		if strings.Contains(host, "]") {
			if bi := strings.LastIndex(host, "]"); bi < i {
				return host[:i]
			}
			return host
		}
		return host[:i]
	}
	return host
}

func canonicalPageHost(mode string, resolver *vhost.Resolver, host, pageRef string) bool {
	if resolver == nil {
		return true
	}
	switch normalizeVhostMode(mode) {
	case VhostModeRedirect:
		return baseHostMatches(host, resolver.BaseDomain())
	default:
		return resolver.IsPageHost(host, pageRef)
	}
}

func pageRedirectTarget(mode string, resolver *vhost.Resolver, r *http.Request, ref, pageRef string) string {
	scheme := requestScheme(r)
	port := requestPort(r)

	switch normalizeVhostMode(mode) {
	case VhostModeRedirect:
		return fmt.Sprintf("%s://%s%s/%s", scheme, resolver.BaseDomain(), port, ref)
	default:
		hash := vhost.PageHash(pageRef)
		return fmt.Sprintf("%s://%s.%s%s/%s", scheme, hash, resolver.BaseDomain(), port, ref)
	}
}
