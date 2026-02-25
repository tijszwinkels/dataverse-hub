package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// InField holds the "in" field. Accepts both string (legacy) and array of
// strings. Always normalizes to a slice internally.
type InField []string

func (f *InField) UnmarshalJSON(data []byte) error {
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*f = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("'in' field must be a string or array of strings")
	}
	*f = []string{s}
	return nil
}

func (f InField) MarshalJSON() ([]byte, error) {
	return json.Marshal([]string(f))
}

// Contains checks if a realm is present in the InField.
func (f InField) Contains(realm string) bool {
	for _, r := range f {
		if r == realm {
			return true
		}
	}
	return false
}

// Envelope is the top-level signed object.
type Envelope struct {
	Is        string          `json:"is,omitempty"`
	In        InField         `json:"in,omitempty"`
	Signature string          `json:"signature"`
	Item      json.RawMessage `json:"item"`
}

// Item is the parsed inner object.
type Item struct {
	In          InField                     `json:"in,omitempty"`
	ID          string                      `json:"id"`
	Pubkey      string                      `json:"pubkey"`
	CreatedAt   string                      `json:"created_at"`
	UpdatedAt   string                      `json:"updated_at,omitempty"`
	Revision    int                         `json:"revision,omitempty"`
	Type        string                      `json:"type,omitempty"`
	Instruction string                      `json:"instruction,omitempty"`
	Relations   map[string][]json.RawMessage `json:"relations,omitempty"`
	Content     json.RawMessage             `json:"content,omitempty"`
}

// ResolveIn returns the authoritative realm list for an envelope+item pair.
// New format: item.In is authoritative.
// Old format: envelope.In is used as fallback.
func ResolveIn(env *Envelope, item *Item) InField {
	if len(item.In) > 0 {
		return item.In
	}
	if len(env.In) > 0 {
		return env.In
	}
	return nil
}

// Ref returns the composite key for this item.
func (it *Item) Ref() string {
	return it.Pubkey + "." + it.ID
}

// Timestamp returns UpdatedAt if set, otherwise CreatedAt.
func (it *Item) Timestamp() (time.Time, error) {
	ts := it.UpdatedAt
	if ts == "" {
		ts = it.CreatedAt
	}
	return time.Parse(time.RFC3339, ts)
}

// RelationRef is a single relation entry. We only need the ref for indexing.
type RelationRef struct {
	Ref string `json:"ref"`
}

// ObjectMeta is stored in the index to avoid re-reading files for filtering.
type ObjectMeta struct {
	Ref             string
	Pubkey          string
	Type            string
	Revision        int
	HasPageRelation bool     // true if item has a "page" relation
	PageRef         string   // first page relation ref (for ETag computation)
	MimeType        string   // content.mime_type for BLOB objects (used for content negotiation)
	IsPublic        bool     // true if "dataverse001" in realms
	Realms          []string // all realm strings from in field
	UpdatedAt       time.Time
}

// RelationEntry maps a target ref back to the source that references it.
type RelationEntry struct {
	SourceRef    string
	RelationType string
}

// Cursor encodes pagination position.
type Cursor struct {
	T   time.Time `json:"t"`
	Ref string    `json:"r"`
}

// ListResponse is the response envelope for list endpoints.
type ListResponse struct {
	Items   []json.RawMessage `json:"items"`
	Cursor  *string           `json:"cursor"`
	HasMore bool              `json:"has_more"`
}

// APIError is returned on errors.
type APIError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// Config holds server configuration.
type Config struct {
	Mode             string // "root" or "proxy" (default: "proxy")
	UpstreamURL      string // upstream hub URL, only used in proxy mode
	Addr             string
	StoreDir         string
	RateLimitPerMin  int
	RateLimitPerDay  int
	DefaultViewerRef string // PAGE ref to use as default object viewer for browsers
	BackupEnabled    bool   // keep old revisions in bk/ (default: true)

	AuthWidgetHost           string        // hostname for auth widget (e.g. "auth.dataverse001.net"), empty to disable
	AuthWidgetAllowedOrigins []string      // origins that may embed the widget (e.g. ["https://dataverse001.net"])
	AuthTokenExpiry          time.Duration // bearer token lifetime (default: 168h = 7 days)
}
