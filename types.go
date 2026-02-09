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
	Ref       string
	Pubkey    string
	Type      string
	UpdatedAt time.Time
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
	Addr            string
	StoreDir        string
	RateLimitPerMin int
	RateLimitPerDay int
}
