package vhost

import (
	"regexp"
	"testing"
)

func TestPageHash_Deterministic(t *testing.T) {
	ref := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"
	h1 := PageHash(ref)
	h2 := PageHash(ref)
	if h1 != h2 {
		t.Errorf("PageHash not deterministic: %q != %q", h1, h2)
	}
}

func TestPageHash_Length(t *testing.T) {
	ref := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"
	h := PageHash(ref)
	if len(h) != 16 {
		t.Errorf("PageHash length = %d, want 16", len(h))
	}
}

func TestPageHash_DNSSafe(t *testing.T) {
	ref := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"
	h := PageHash(ref)
	if !regexp.MustCompile(`^[0-9a-f]+$`).MatchString(h) {
		t.Errorf("PageHash %q contains non-hex characters", h)
	}
}

func TestPageHash_DifferentRefs(t *testing.T) {
	ref1 := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.ea96b9f6-1234-5678-9abc-def012345678"
	ref2 := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.bbbbbbbb-1234-5678-9abc-def012345678"
	h1 := PageHash(ref1)
	h2 := PageHash(ref2)
	if h1 == h2 {
		t.Errorf("PageHash collision: %q and %q both produce %q", ref1, ref2, h1)
	}
}
