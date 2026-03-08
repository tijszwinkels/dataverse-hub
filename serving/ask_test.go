package serving

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tijszwinkels/dataverse-hub/vhost"
)

func TestTLSAskHandler(t *testing.T) {
	// Build a resolver with a known hash subdomain
	resolver := vhost.NewResolver("dataverse001.net", 5*time.Minute, func(host string) ([]string, error) {
		// Mock TXT lookup: _dv.social.dataverse001.net returns a ref
		if host == "_dv.social.dataverse001.net" {
			return []string{"AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"}, nil
		}
		// Mock custom domain
		if host == "_dv.dataverse.social" {
			return []string{"dv1-page=AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"}, nil
		}
		return nil, nil
	})

	// Add a hash subdomain
	pageRef := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"
	resolver.UpdateHashMap(map[string]string{
		vhost.PageHash(pageRef): pageRef,
	})

	handler := TLSAskHandler(resolver)

	tests := []struct {
		name   string
		domain string
		want   int
	}{
		{"hash subdomain", vhost.PageHash(pageRef) + ".dataverse001.net", http.StatusOK},
		{"named subdomain via TXT", "social.dataverse001.net", http.StatusOK},
		{"custom domain via TXT", "dataverse.social", http.StatusOK},
		{"bogus subdomain", "update.social.dataverse001.net", http.StatusForbidden},
		{"scanner garbage", "update.auth.c7574b078d5d40e7.dataverse001.net", http.StatusForbidden},
		{"bare IP", "172.18.0.2", http.StatusForbidden},
		{"bare base domain", "dataverse001.net", http.StatusForbidden},
		{"missing domain param", "", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/ask"
			if tt.domain != "" {
				url += "?domain=" + tt.domain
			}
			req := httptest.NewRequest("GET", url, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("domain=%q: got %d, want %d", tt.domain, rec.Code, tt.want)
			}
		})
	}
}

func TestTLSAskHandler_NilResolver(t *testing.T) {
	handler := TLSAskHandler(nil)
	req := httptest.NewRequest("GET", "/ask?domain=anything.dataverse001.net", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("nil resolver: got %d, want %d", rec.Code, http.StatusForbidden)
	}
}
