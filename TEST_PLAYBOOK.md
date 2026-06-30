# Test Playbook — SHARED_REALM deterministic addressing (realm/realmid.go)

Copy-paste-ready commands to exercise the new `realm.RealmID` / `realm.RealmRef`
helpers and their UUIDv5 determinism. Run these in any terminal in the workspace
(`/work`).

> **Scope of this change:** only `realm/realmid.go` + `realm/realmid_test.go`.
> No hub wiring yet — these tests cover the pure addressing layer.

---

## 0. One-shot sanity (build + tests)

```bash
cd /work
go build ./... && go test ./realm/ -v -count=1
```

Expected: `PASS`, `ok github.com/tijszwinkels/dataverse-hub/realm`. No build errors.

---

## 1. Interoperability — Go matches Python stdlib `uuid.uuid5`

The whole point of UUIDv5 is that *any* implementation agrees. Run the Go test
that pins to Python's output, then confirm by hand:

```bash
cd /work
go test ./realm/ -run TestRealmID_MatchesStandardUUIDv5 -v
```

Then cross-check independently in Python:

```bash
python3 - <<'PY'
import uuid
NS = uuid.UUID("00000000-0000-0000-0000-000000000000")
for r in [
    "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam",
    "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.MyTeam",
    "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.OtherTeam",
]:
    print(r, "->", uuid.uuid5(NS, r))
PY
```

Expected Go values (must equal Python exactly):
- `AcmeTeam`  -> `6bb7d6cc-1556-5a76-a910-edc802d4a2b7`
- `MyTeam`    -> `56390347-d50c-5dce-970f-b0a0c7abb6de`
- `OtherTeam` -> `710628b0-3abc-5f46-9a09-9bceab872a55`

---

## 2. Determinism + distinctness

```bash
cd /work
go test ./realm/ -run TestRealmID_DeterministicAndDistinct -v
```

Expected: same realm -> same id; different realms -> different ids.

---

## 3. Valid UUIDv5 shape (version + variant bits)

```bash
cd /work
go test ./realm/ -run TestRealmID_VersionAndVariant -v
```

Expected: version nibble = `5` at position 14; variant nibble in `[8,9,a,b]` at position 19.

Also eyeball the shape with a tiny Go program:

```bash
cd /work && cat > /tmp/shape.go <<'GO'
package main
import ("fmt"; "github.com/tijszwinkels/dataverse-hub/realm")
func main(){
  id := realm.RealmID("AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam")
  fmt.Println(id)
  // 8-4-4-4-12, lowercase, version nibble '5'
}
GO
go run /tmp/shape.go
```

Expected: `6bb7d6cc-1556-5a76-a910-edc802d4a2b7` (36 chars, hyphens at 8/13/18/23).

---

## 4. RealmRef = owner + "." + RealmID(realm)

```bash
cd /work
go test ./realm/ -run TestRealmRef -v
```

Manual check:

```bash
cd /work && cat > /tmp/ref.go <<'GO'
package main
import ("fmt"; "github.com/tijszwinkels/dataverse-hub/realm")
func main(){
  r := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam"
  ref, err := realm.RealmRef(r)
  fmt.Println(ref, err)
}
GO
go run /tmp/ref.go
```

Expected: `AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.6bb7d6cc-1556-5a76-a910-edc802d4a2b7  <nil>`

---

## 5. Rejection of non-owner-prefixed realms

```bash
cd /work
go test ./realm/ -run TestRealmRef_RejectsNonOwnerPrefixed -v
```

Manual:

```bash
cd /work && cat > /tmp/bad.go <<'GO'
package main
import ("fmt"; "github.com/tijszwinkels/dataverse-hub/realm")
func main(){
  for _, r := range []string{"not-a-pubkey.Team", "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ"} {
    ref, err := realm.RealmRef(r)
    fmt.Printf("%-50q ref=%q err=%v\n", r, ref, err)
  }
}
GO
go run /tmp/bad.go
```

Expected: both return a non-nil error and empty ref. (Decision 3: prefix rule.)

---

## 6. Names with embedded dots

The Name part may contain dots; only the first dot splits the owner. Confirm a
multi-dot realm hashes consistently and the owner is the pubkey only:

```bash
cd /work && cat > /tmp/dots.go <<'GO'
package main
import ("fmt"; "github.com/tijszwinkels/dataverse-hub/realm")
func main(){
  r := "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.team.alpha.beta"
  ref, err := realm.RealmRef(r)
  fmt.Println("ref:", ref, "err:", err)
  fmt.Println("id :", realm.RealmID(r))
  // Must be stable across runs:
  fmt.Println("id2:", realm.RealmID(r))
}
GO
go run /tmp/dots.go
```

Expected: no error; `ref` starts with the 44-char pubkey; two calls produce the
same `id`.

---

## 7. Full module still healthy (nothing else broke)

```bash
cd /work
go vet ./...
go test ./... -count=1
```

Expected: all packages pass; `realm/` includes the new tests.

---

## 8. (Optional) Fuzz for collisions / non-determinism

```bash
cd /work && cat > /tmp/fuzz_test.go <<'GO'
package realm
import "testing"
func FuzzRealmID(f *testing.F){
  f.Add("AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.x")
  f.Fuzz(func(t *testing.T, realm string){
    a := RealmID(realm); b := RealmID(realm)
    if a != b { t.Fatalf("non-deterministic: %q -> %q vs %q", realm, a, b) }
    if len(a) != 36 { t.Fatalf("bad shape %q len=%d", a, len(a)) }
    if a[14] != '5' { t.Fatalf("bad version nibble in %q", a) }
  })
}
GO
cp /tmp/fuzz_test.go realm/zz_fuzz_test.go
go test ./realm/ -fuzz=FuzzRealmID -fuzztime=10s
rm realm/zz_fuzz_test.go
```

Expected: no failures over the 10s fuzz run (PASS / "no failing corpus").

---

## Cleanup

```bash
rm -f /tmp/shape.go /tmp/ref.go /tmp/bad.go /tmp/dots.go /tmp/fuzz_test.go
```
