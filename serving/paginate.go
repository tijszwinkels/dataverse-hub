package serving

import (
	"encoding/base64"
	"encoding/json"
	"log"

	"github.com/dataverse/hub/object"
	"github.com/dataverse/hub/storage"
)

// paginateAndLoad applies cursor-based pagination to metas, loads data from store,
// and returns items, refs, next cursor, and hasMore flag.
func paginateAndLoad(store *storage.Store, metas []object.ObjectMeta, cursor *object.Cursor, limit int) ([]json.RawMessage, []string, *string, bool) {
	// Apply cursor: skip items until we pass the cursor position
	if cursor != nil {
		idx := 0
		for idx < len(metas) {
			m := metas[idx]
			if m.UpdatedAt.Before(cursor.T) || (m.UpdatedAt.Equal(cursor.T) && m.Ref < cursor.Ref) {
				break
			}
			idx++
		}
		metas = metas[idx:]
	}

	hasMore := len(metas) > limit
	if hasMore {
		metas = metas[:limit]
	}

	var items []json.RawMessage
	var refs []string
	for _, m := range metas {
		data, err := store.Read(m.Ref)
		if err != nil || data == nil {
			log.Printf("WARN: paginate skip %s: read error or missing", m.Ref)
			continue
		}
		item := json.RawMessage(data)
		if m.Type == "BLOB" {
			item = stripBlobData(item)
		}
		items = append(items, item)
		refs = append(refs, m.Ref)
	}

	var nextCursor *string
	if hasMore && len(metas) > 0 {
		last := metas[len(metas)-1]
		c := object.Cursor{T: last.UpdatedAt, Ref: last.Ref}
		encoded, _ := json.Marshal(c)
		s := base64.RawURLEncoding.EncodeToString(encoded)
		nextCursor = &s
	}

	return items, refs, nextCursor, hasMore
}

// enrichWithInboundCounts adds _inbound_counts to each item in the list.
func enrichWithInboundCounts(index *storage.Index, items []json.RawMessage, refs []string) []json.RawMessage {
	enriched := make([]json.RawMessage, len(items))
	for i, item := range items {
		counts := index.GetInboundCounts(refs[i])
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			log.Printf("WARN: enrich skip %s: unmarshal: %v", refs[i], err)
			enriched[i] = item
			continue
		}
		countsJSON, _ := json.Marshal(counts)
		obj["_inbound_counts"] = countsJSON
		result, err := json.Marshal(obj)
		if err != nil {
			log.Printf("WARN: enrich skip %s: marshal: %v", refs[i], err)
			enriched[i] = item
			continue
		}
		enriched[i] = result
	}
	return enriched
}
