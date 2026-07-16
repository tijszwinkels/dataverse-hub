package serving

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/tijszwinkels/dataverse-hub/object"
	"github.com/tijszwinkels/dataverse-hub/realm"
	"github.com/tijszwinkels/dataverse-hub/storage"
	"github.com/tijszwinkels/dataverse-hub/vhost"
)

type viewerSource uint8

const (
	viewerNone viewerSource = iota
	viewerInline
	viewerDirect
	viewerTypeDefault
	viewerHubDefault
)

// viewerResolution is an HTML viewer selected for the original requested
// object. pageRef identifies the PAGE's origin-isolation boundary; it never
// replaces the requested ref in URLs or page context.
type viewerResolution struct {
	source       viewerSource
	pageRef      string
	html         string
	typeRev      int
	pageRev      int
	defaultRev   int
	requiresAuth bool
}

func (v viewerResolution) available() bool {
	return v.source != viewerNone && v.html != ""
}

func (v viewerResolution) etagSuffix() string {
	switch v.source {
	case viewerInline:
		return "-html"
	case viewerDirect:
		return fmt.Sprintf("-p%d-html", v.pageRev)
	case viewerTypeDefault:
		return fmt.Sprintf("-t%d-p%d-html", v.typeRev, v.pageRev)
	case viewerHubDefault:
		return fmt.Sprintf("-g%d-p%d-html", v.defaultRev, v.pageRev)
	default:
		return "-html"
	}
}

type viewerResolver struct {
	store         *storage.Store
	index         *storage.Index
	requestedRef  string
	requestedData []byte
	authPubkey    string
	enforceAccess bool
}

func newViewerResolver(store *storage.Store, index *storage.Index, requestedRef string, requestedData []byte, authPubkey string) viewerResolver {
	return viewerResolver{
		store:         store,
		index:         index,
		requestedRef:  requestedRef,
		requestedData: requestedData,
		authPubkey:    authPubkey,
		enforceAccess: true,
	}
}

func (r viewerResolver) withoutAccessCheck() viewerResolver {
	r.enforceAccess = false
	return r
}

// primary resolves only object-owned viewers: inline PAGE, direct page, then
// one TYPE-level default. It deliberately never recurses through type_def.
func (r viewerResolver) primary() viewerResolution {
	env, item, err := object.ParseEnvelope(r.requestedData)
	if err != nil {
		log.Printf("WARN: viewer %s: parse requested object: %v", r.requestedRef, err)
		return viewerResolution{}
	}
	if item.Ref() != r.requestedRef {
		log.Printf("WARN: viewer %s: stored object ref is %s", r.requestedRef, item.Ref())
		return viewerResolution{}
	}

	if item.Type == "PAGE" {
		return viewerResolution{
			source:       viewerInline,
			pageRef:      r.requestedRef,
			html:         extractHTML(item),
			pageRev:      item.Revision,
			requiresAuth: !realm.IsPublicObject(object.ResolveIn(env, item)),
		}
	}

	visited := map[string]bool{r.requestedRef: true}
	if pageRef := firstRelationRef(item, "page", r.requestedRef); pageRef != "" {
		if page, ok := r.loadPage(pageRef, visited); ok {
			page.source = viewerDirect
			return page
		}
	}

	typeRef := firstRelationRef(item, "type_def", r.requestedRef)
	if typeRef == "" || visited[typeRef] {
		return viewerResolution{}
	}
	visited[typeRef] = true
	typeItem, typePublic, ok := r.loadItem(typeRef, "TYPE")
	if !ok {
		return viewerResolution{}
	}
	typePageRef := firstRelationRef(typeItem, "page", typeRef)
	if typePageRef == "" {
		return viewerResolution{}
	}
	page, ok := r.loadPage(typePageRef, visited)
	if !ok {
		return viewerResolution{}
	}
	page.source = viewerTypeDefault
	page.typeRev = typeItem.Revision
	page.requiresAuth = page.requiresAuth || !typePublic
	return page
}

// hubDefault resolves the configured generic viewer with its existing
// semantics: the configured object may be a PAGE or point directly at one.
// Type inheritance is intentionally not recursive here.
func (r viewerResolver) hubDefault(defaultRef string) viewerResolution {
	if defaultRef == "" || defaultRef == r.requestedRef {
		return viewerResolution{}
	}
	defaultItem, defaultPublic, ok := r.loadItem(defaultRef, "")
	if !ok {
		return viewerResolution{}
	}
	if defaultItem.Type == "PAGE" {
		html := extractHTML(defaultItem)
		if html == "" {
			return viewerResolution{}
		}
		return viewerResolution{
			source:       viewerHubDefault,
			pageRef:      defaultRef,
			html:         html,
			pageRev:      defaultItem.Revision,
			defaultRev:   defaultItem.Revision,
			requiresAuth: !defaultPublic,
		}
	}
	pageRef := firstRelationRef(defaultItem, "page", defaultRef)
	if pageRef == "" {
		return viewerResolution{}
	}
	page, ok := r.loadPage(pageRef, map[string]bool{r.requestedRef: true, defaultRef: true})
	if !ok {
		return viewerResolution{}
	}
	page.source = viewerHubDefault
	page.defaultRev = defaultItem.Revision
	page.requiresAuth = page.requiresAuth || !defaultPublic
	return page
}

func (r viewerResolver) loadPage(ref string, visited map[string]bool) (viewerResolution, bool) {
	if ref == "" || visited[ref] {
		return viewerResolution{}, false
	}
	visited[ref] = true
	item, public, ok := r.loadItem(ref, "PAGE")
	if !ok {
		return viewerResolution{}, false
	}
	html := extractHTML(item)
	if html == "" {
		log.Printf("WARN: viewer %s: PAGE %s has no HTML", r.requestedRef, ref)
		return viewerResolution{}, false
	}
	return viewerResolution{pageRef: ref, html: html, pageRev: item.Revision, requiresAuth: !public}, true
}

func (r viewerResolver) loadItem(ref, requiredType string) (*object.Item, bool, bool) {
	data, err := r.store.Read(ref)
	if err != nil {
		log.Printf("WARN: viewer %s: read dependency %s: %v", r.requestedRef, ref, err)
		return nil, false, false
	}
	if data == nil {
		log.Printf("WARN: viewer %s: dependency %s not found", r.requestedRef, ref)
		return nil, false, false
	}
	env, item, err := object.ParseEnvelope(data)
	if err != nil {
		log.Printf("WARN: viewer %s: parse dependency %s: %v", r.requestedRef, ref, err)
		return nil, false, false
	}
	if item.Ref() != ref {
		log.Printf("WARN: viewer %s: dependency %s contains ref %s", r.requestedRef, ref, item.Ref())
		return nil, false, false
	}
	if requiredType != "" && item.Type != requiredType {
		log.Printf("WARN: viewer %s: dependency %s is %s, want %s", r.requestedRef, ref, item.Type, requiredType)
		return nil, false, false
	}
	realms := object.ResolveIn(env, item)
	if r.enforceAccess && !realm.CanRead([]string(realms), r.authPubkey, r.index.Resolver()) {
		log.Printf("WARN: viewer %s: dependency %s is not readable by requester", r.requestedRef, ref)
		return nil, false, false
	}
	return item, realm.IsPublicObject(realms), true
}

func firstRelationRef(item *object.Item, relation, ownerRef string) string {
	entries := item.Relations[relation]
	if len(entries) == 0 {
		return ""
	}
	var rel object.RelationRef
	if err := json.Unmarshal(entries[0], &rel); err != nil {
		log.Printf("WARN: viewer %s: invalid %s relation: %v", ownerRef, relation, err)
		return ""
	}
	if rel.Ref == "" {
		log.Printf("WARN: viewer %s: %s relation has no ref", ownerRef, relation)
		return ""
	}
	return rel.Ref
}

type representationKind uint8

const (
	representationJSON representationKind = iota
	representationBlob
	representationHTML
)

type selectedRepresentation struct {
	kind     representationKind
	viewer   viewerResolution
	blobMIME string
	blobData []byte
}

// indexedNonHTMLValidator preserves the existing no-read 304 path for JSON and
// raw-BLOB negotiation. HTML must resolve dependencies before its validator is
// known, so callers continue into the centralized resolver for those requests.
func indexedNonHTMLValidator(req *http.Request, meta object.ObjectMeta) (string, bool) {
	if acceptsHTML(req) {
		return "", false
	}
	kind := representationJSON
	if meta.Type == "BLOB" && meta.MimeType != "" && acceptsMimeType(req, meta.MimeType) {
		kind = representationBlob
	}
	return (selectedRepresentation{kind: kind}).etag(meta.Revision), true
}

func selectNegotiatedRepresentation(req *http.Request, resolver viewerResolver, defaultViewerRef string) selectedRepresentation {
	if acceptsHTML(req) {
		if viewer := resolver.primary(); viewer.available() {
			return selectedRepresentation{kind: representationHTML, viewer: viewer}
		}
	}
	if blob, ok := blobContent(resolver.requestedData); ok && acceptsMimeType(req, blob.mimeType) {
		if raw, ok := blob.decode(); ok {
			return selectedRepresentation{kind: representationBlob, blobMIME: blob.mimeType, blobData: raw}
		}
	}
	if acceptsHTML(req) {
		if viewer := resolver.hubDefault(defaultViewerRef); viewer.available() {
			return selectedRepresentation{kind: representationHTML, viewer: viewer}
		}
	}
	return selectedRepresentation{kind: representationJSON}
}

func selectPageRepresentation(resolver viewerResolver, defaultViewerRef string) (selectedRepresentation, bool) {
	if viewer := resolver.primary(); viewer.available() {
		return selectedRepresentation{kind: representationHTML, viewer: viewer}, true
	}
	if viewer := resolver.hubDefault(defaultViewerRef); viewer.available() {
		return selectedRepresentation{kind: representationHTML, viewer: viewer}, true
	}
	return selectedRepresentation{}, false
}

func (s selectedRepresentation) etag(revision int) string {
	switch s.kind {
	case representationBlob:
		return fmt.Sprintf("\"%d-blob\"", revision)
	case representationHTML:
		return fmt.Sprintf("\"%d%s\"", revision, s.viewer.etagSuffix())
	default:
		return fmt.Sprintf("\"%d\"", revision)
	}
}

func (s selectedRepresentation) write(w http.ResponseWriter, data []byte, baseDomain string) {
	switch s.kind {
	case representationBlob:
		writeBlobBytes(w, s.blobMIME, s.blobData)
	case representationHTML:
		writePageHTML(w, s.viewer.html, baseDomain)
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func (s selectedRepresentation) setCacheHeaders(w http.ResponseWriter) {
	if s.kind == representationHTML && s.viewer.requiresAuth {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
}

func (s selectedRepresentation) isolatedPageRef() string {
	if s.kind != representationHTML || s.viewer.source == viewerHubDefault {
		return ""
	}
	return s.viewer.pageRef
}

func servePrivateViewerLogin(w http.ResponseWriter, req *http.Request, resolver viewerResolver, vhostResolver *vhost.Resolver, vhostMode, ref string) bool {
	if vhostResolver == nil || !acceptsHTML(req) {
		return false
	}
	viewer := resolver.withoutAccessCheck().primary()
	if !viewer.available() {
		return false
	}
	if vhostRedirect(w, req, vhostResolver, vhostMode, ref, viewer.pageRef, "") {
		return true
	}
	servePrivatePageLogin(w)
	return true
}
