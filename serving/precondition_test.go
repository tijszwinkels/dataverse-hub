package serving

import (
	"testing"

	"github.com/tijszwinkels/dataverse-hub/object"
)

func TestEvaluateIfMatch(t *testing.T) {
	meta := func(rev int) object.ObjectMeta { return object.ObjectMeta{Revision: rev} }

	cases := []struct {
		name   string
		header string
		meta   object.ObjectMeta
		exists bool
		want   ifMatchResult
	}{
		{"absent", "", meta(5), true, ifMatchAbsent},
		{"absent whitespace", "   ", meta(5), true, ifMatchAbsent},
		{"exact match", `"5"`, meta(5), true, ifMatchPass},
		{"mismatch", `"4"`, meta(5), true, ifMatchFail},
		{"concrete on missing", `"5"`, object.ObjectMeta{}, false, ifMatchFail},
		{"star on existing", "*", meta(5), true, ifMatchPass},
		{"star on missing", "*", object.ObjectMeta{}, false, ifMatchFail},
		{"list contains match", `"3", "5", "7"`, meta(5), true, ifMatchPass},
		{"list no match", `"3", "7"`, meta(5), true, ifMatchFail},
		{"weak tag never matches", `W/"5"`, meta(5), true, ifMatchFail},
		{"trailing space trimmed", `"5" `, meta(5), true, ifMatchPass},
		{"revision zero", `"0"`, meta(0), true, ifMatchPass},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateIfMatch(tc.header, tc.meta, tc.exists); got != tc.want {
				t.Errorf("evaluateIfMatch(%q, rev=%d, exists=%v) = %d, want %d",
					tc.header, tc.meta.Revision, tc.exists, got, tc.want)
			}
		})
	}
}

func TestRevisionETag(t *testing.T) {
	if got, want := revisionETag(7), `"7"`; got != want {
		t.Errorf("revisionETag(7) = %q, want %q", got, want)
	}
}
