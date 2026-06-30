# Shared-Realm Access Control — A New instructionGraph001 Type

## Goal

Define a new graph-native type that declares which pubkeys have read access to a
shared realm, so the hub can decide shared-realm access from the graph itself
(not only from hub-operator TOML config).

---

## Current state

- Realm membership is declared in the signed `item.in` array. The signer chooses
  which realms an object belongs to. **Realm *semantics* (who can read, how it
  propagates) are explicitly left to deployments** — the format spec does not
  define them.
- The hub implements "shared realm" membership out-of-band, in `hub.toml`:

  ```toml
  [realms."AxyU5_...MyTeam"]
  members = ["AxyU5_...", "BzxY7_..."]
  ```

- `realm.SharedRealms` (`realm/shared.go`) is an in-memory `map[realmName][]pubkey`,
  loaded at startup / on `SIGHUP`. `realm.CanRead` consults it for read gating.

**Problem:** membership is not signed, not portable, not discoverable by other
hubs. It contradicts the self-describing, transport-agnostic intent of the
dataverse.

---

## Proposed type

A new object type, `item.type = "REALM"`, that names a shared realm and lists
its members as relations.

### Object shape

```json
{
  "is": "instructionGraph001",
  "signature": "<base64 ECDSA over item>",
  "item": {
    "in": ["dataverse001", "<owner-pubkey>"],
    "ref": "<owner-pubkey>.<uuid>",
    "id": "<uuid>",
    "pubkey": "<owner-pubkey>",
    "created_at": "<iso8601>",
    "updated_at": "<iso8601>",
    "revision": 1,
    "type": "REALM",
    "name": "Acme Team",
    "instruction": "A shared realm. Listed members have read access to objects carrying this realm name in item.in.",
    "content": {
      "realm": "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.AcmeTeam"
    },
    "relations": {
      "member": [
        { "ref": "AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.346bef5e-..." },
        { "ref": "BzxY7_...a1b2c3d4..." }
      ]
    },
    "rights": { "license": "CC0-1.0", "ai_training_allowed": true }
  }
}
```

### Key design choices (for discussion)

1. **Realm identity lives in `content.realm`.** The realm string that other
   objects put in their `item.in` is stored here (not derived from `item.name`
   or the object's ref). This decouples the human-readable `name` from the
   access-control identifier, and lets a realm object be renamed without
   invalidating every member object.

   - Alternative: derive the realm name from the signer + a slug, e.g.
     `{owner-pubkey}.{name}`. Pro: no separate field, matches the existing
     `{owner-pubkey}.{Name}` convention in the README. Con: rename breaks it.

2. **Members are `relations.member`.** Each entry's `ref` resolves to a
   composite key `{pubkey}.{uuid}`. Question: must that target object *exist*
   (a real IDENTITY object), or is the *pubkey portion* of the ref sufficient
   for access decisions? Two sub-options:
   - **(a) Pubkey-only:** access is granted to the pubkey embedded in the
     member ref. The hub just needs `ref.split('.')[0]`. No fetch needed.
     Simple, offline-friendly.
   - **(b) Require IDENTITY:** the member ref must point to an IDENTITY object
     the hub can resolve; access is granted to that identity's pubkey. Stronger
     linking / revocation story, but introduces a fetch dependency.

3. **Ownership / authorization to define members.** Who is allowed to author
   a REALM object for realm `X`? Options:
   - **Prefix rule:** realm string must start with `{signer-pubkey}.`, so the
     owner of a pubkey namespace "owns" realms under it. Matches the existing
     README convention. Simple, self-consistent with composite-key identity.
   - **First-writer wins / revision-highest wins:** whoever created the
     lowest-revision object for a realm name owns it; later conflicting objects
     are ignored unless higher revision from the same owner. More flexible but
     needs the full history.
   - **Explicit `owner` relation** to the root identity, separate from members.

4. **Update & conflict resolution.** Reuse the existing `revision` rule
   (highest revision wins on sync). One REALM object per `content.realm`
   string? Or one per composite key `{owner-pubkey}.{uuid}` with multiple
   objects allowed to claim the same realm name (and the hub picks a winner)?
   - If we tie realm identity to `content.realm` (option 1), we need a rule to
     pick the canonical object among possibly several authors.

5. **Revocation / removal.** Removing a pubkey from `relations.member` in a
   higher-revision REALM object revokes them. Do we also want an explicit
   `blocked` relation for deny-listing that overrides `member`?

6. **Propagation / `item.in` for the REALM object itself.** Should REALM
   objects be in `dataverse001` (globally propagated, world-readable — so any
   hub can discover the access list), or in the owner's identity realm (private,
   synced only among hubs the owner pushes to)? Trade-off: discoverability vs.
   leaking the member roster.

7. **Hub precedence: graph vs. TOML.** When the hub resolves membership for a
   realm, in what order does it consult sources?
   - Graph REALM object (signed, authoritative) → TOML override (operator
     force-add/remove) → deny by default.
   - Or TOML takes precedence so operators retain an emergency kill switch?

8. **Caching & hot reload.** The hub already indexes objects and reloads config
   on SIGHUP. REALM objects would be resolved at index-build time and on PUT of
   a new REALM revision. No per-request fetch needed; membership lookup stays
   O(1) like today.

---

## Open questions for you

- Type name: `REALM`, `SHARED_REALM`, `ACCESS_LIST`, or something else?
- Member relation name: `member`, `members`, `grants_access`?
- Option 1 (content.realm) vs. deriving realm name from owner+name?
- Member refs: pubkey-only (a) vs. require resolvable IDENTITY (b)?
- Ownership: prefix rule vs. first-writer vs. explicit owner relation?
- Should REALM objects be global (`dataverse001`) or owner-private?
- TOML precedence vs. graph precedence on conflicts?
- Revocation: just edit members (higher revision), or add a `blocked` relation?

---

## Hub implementation (phase 2, after we lock the type)

- New `realm/graph.go`: parse REALM objects from the store into a
  `GraphSharedRealms` that satisfies the same interface `SharedRealms` exposes
  (`IsSharedRealm`, `IsMember`, `RealmsForPubkey`, `Count`).
- Index hook: when a `type=REALM` object is PUT/rebuilt, (re)compute membership
  for its `content.realm`.
- `realm.CanRead` / `ValidateRealmsForPut` consult the merged source
  (graph + TOML override) instead of TOML alone.
- `GET /auth/realms` lists realms the caller is a member of — sourced from
  both graph REALM objects and TOML.
- Keep TOML as optional override for hub operators.
