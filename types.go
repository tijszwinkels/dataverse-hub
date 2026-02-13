package main

import (
	"encoding/json"
	"time"
)

// Envelope is the top-level signed dataverse001 object.
type Envelope struct {
	In        string          `json:"in"`
	Signature string          `json:"signature"`
	Item      json.RawMessage `json:"item"`
}

// Item is the parsed inner object.
type Item struct {
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
	HasPageRelation bool   // true if item has a "page" relation
	PageRef         string // first page relation ref (for ETag computation)
	MimeType        string // content.mime_type for BLOB objects (used for content negotiation)
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
}
