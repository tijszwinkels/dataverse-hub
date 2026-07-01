package realm

import (
	"sort"
	"testing"
)

func TestMerged_IsSharedRealm_EitherSource(t *testing.T) {
	toml := NewSharedRealms()
	toml.Load(map[string][]string{"tomlRealm": {"alice"}})
	graph := NewGraphSharedRealms()
	// Add a graph realm by building an item via the test helper.
	graph.Add(sharedRealmItem(t, testPK, testPK+".GraphRealm", []string{testPK2}, 1))

	m := NewMerged(graph, toml)

	if !m.IsSharedRealm("tomlRealm") {
		t.Error("TOML-only realm should be known")
	}
	if !m.IsSharedRealm(testPK + ".GraphRealm") {
		t.Error("graph-only realm should be known")
	}
	if m.IsSharedRealm("unknown") {
		t.Error("unknown realm should not be known")
	}
}

func TestMerged_IsMember_EitherSource(t *testing.T) {
	toml := NewSharedRealms()
	toml.Load(map[string][]string{"R": {"tomlMember"}})
	graph := NewGraphSharedRealms()
	graph.Add(sharedRealmItem(t, testPK, testPK+".R", []string{testPK2}, 1))

	m := NewMerged(graph, toml)

	// TOML grants tomlMember; graph grants testPK2.
	if !m.IsMember("R", "tomlMember") {
		t.Error("TOML-granted member should pass")
	}
	if !m.IsMember(testPK+".R", testPK2) {
		t.Error("graph-granted member should pass")
	}
}

func TestMerged_IsMember_TOMLCannotDenyGraph(t *testing.T) {
	// Documented limitation: TOML is additive-only, not a deny-list. A member
	// granted by the graph is still a member even if TOML doesn't list them.
	graph := NewGraphRealmsWithMember(t, testPK+".R", testPK2)
	toml := NewSharedRealms()
	toml.Load(map[string][]string{testPK + ".R": {"someoneElse"}}) // no testPK2

	m := NewMerged(graph, toml)
	if !m.IsMember(testPK+".R", testPK2) {
		t.Error("graph-granted member must still pass: TOML cannot deny")
	}
}

func TestMerged_RealmsForPubkey_UnionDeduped(t *testing.T) {
	// Same realm in both sources, plus a unique one in each.
	realmShared := testPK + ".Shared"
	toml := NewSharedRealms()
	toml.Load(map[string][]string{
		realmShared:      {testPK2}, // also in graph
		"tomlOnly":       {testPK2},
	})
	graph := NewGraphRealmsWithMember(t, realmShared, testPK2)
	graph.Add(sharedRealmItem(t, testPK, testPK+".graphOnly", []string{testPK2}, 1))

	m := NewMerged(graph, toml)
	got := m.RealmsForPubkey(testPK2)
	sort.Strings(got)

	want := []string{realmShared, testPK + ".graphOnly", "tomlOnly"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("RealmsForPubkey = %v, want %v (deduped union)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RealmsForPubkey[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestMerged_NilComponents(t *testing.T) {
	m := NewMerged(nil, nil)
	if m.IsSharedRealm("x") || m.IsMember("x", "y") || m.Count() != 0 {
		t.Error("nil Merged should be empty")
	}
	if r := m.RealmsForPubkey("y"); len(r) != 0 {
		t.Errorf("nil Merged RealmsForPubkey = %v, want []", r)
	}
}

// NewGraphRealmsWithMember is a test helper building a graph with one realm
// containing the given member pubkey.
func NewGraphRealmsWithMember(t *testing.T, realmName, member string) *GraphSharedRealms {
	t.Helper()
	g := NewGraphSharedRealms()
	g.Add(sharedRealmItem(t, testPK, realmName, []string{member}, 1))
	return g
}
