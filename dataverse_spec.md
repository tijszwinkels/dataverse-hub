# instructionGraph001 — Data Format Specification

A signed, self-describing graph data format. Each object is a self-contained JSON fragment that can be verified, interpreted, and linked to other objects by any agent that encounters it.

instructionGraph001 defines the **wire format** — how objects are structured, signed, identified, and related. It says nothing about where objects live, how they propagate, or what realms they belong to. Those concerns are left to specific deployments (like dataverse001).

## Object Format

```json
{
  "is": "instructionGraph001",
  "signature": "<base64>",
  "item": {
    "in": ["<realm>"],
    "ref": "<pubkey>.<uuid>",
    "id": "<uuid>",
    "pubkey": "<compressed-raw-pubkey-base64url>",
    "created_at": "<iso8601>",
    "updated_at": "<iso8601>",
    "revision": 0,
    "type": "POST",
    "name": "My First Post",
    "instruction": "A post. Display title and body.",
    "rights": {
      "license": "CC0-1.0",
      "ai_training_allowed": true
    },
    "relations": {
      "in_application": [{"ref": "<pubkey>.<uuid>"}],
      "in_subcommunity": [{"ref": "<pubkey>.<uuid>"}],
      "author": [{"ref": "<pubkey>.<uuid>"}]
    },
    "content": {
      "title": "Hello World",
      "body": "First post!"
    }
  }
}
```

## Envelope vs Item

An instructionGraph001 object has two layers:

- **Envelope** (unsigned): Contains `is` (format identifier) and `signature`. This is what generic tooling uses to recognize and verify the object.
- **Item** (signed): The actual payload. Everything inside `item` is covered by the signature.

The `is` field identifies the data format. The `signature` covers the canonical JSON of `item`. This envelope pattern means you can add unsigned metadata (like transport hints) without breaking the signature.

## Object Identity

Objects are uniquely identified by the composite key `(pubkey, id)`. This prevents UUID squatting — you can only create objects under your own pubkey namespace.

### Composite Key Format

The composite key is expressed as a single string: `{pubkey}.{id}`

Example: `AxyU5_5vWmP2tO_klN4UpbZzRsuJEvJTrdwdg_gODxZJ.346bef5e-94ff-4f7a-bcf6-d78ae1e1541c`

This format:
- Uses `.` as delimiter (URL-safe, filesystem-safe, not in base64url or UUIDs)
- Enables direct lookup by filename, URL path, or database key

Every object carries its own composite key in the `ref` field (`item.ref`). This matches exactly what other objects use to point at it in relations.

## Required Fields

**Envelope (unsigned):**

- **is**: Must be `"instructionGraph001"`. Format identifier for parsers and scanners.
- **signature**: ECDSA signature over the canonical JSON of `item`.

**Item (signed):**

- **item.in**: Array of realm strings. The signer controls which realms/databases the object belongs to. This is part of the signed payload.
- **item.ref**: Composite key `{pubkey}.{id}` — the object's own reference.
- **item.id**: UUID.
- **item.pubkey**: Creator's public key (compressed raw EC point, base64url encoded).
- **item.created_at**: ISO8601 timestamp.

## Optional Fields

- **item.name**: Short human-readable label.
- **item.updated_at**: Timestamp of last modification.
- **item.revision**: Integer counter, incremented on update. Higher revision wins on sync.
- **item.type**: Application-level type hint (e.g. `POST`, `COMMENT`, `IDENTITY`, `TYPE`).
- **item.instruction**: Free-text field telling agents how to interpret/display this object. The core mechanism for self-describing data.
- **item.rights**: Object grouping licensing and usage permissions (see Licensing & AI Training below).
- **item.relations**: Named arrays of relation entries (see below).
- **item.content**: Free-form payload (title, body, structured data, etc.).

## Relations

Relations are named arrays of objects with a `ref` field containing a composite key:

```json
"relations": {
  "author": [{"ref": "AxyU5_...346bef5e..."}],
  "replies_to": [{"ref": "BzxY7_...a1b2c3d4...", "title": "Parent post"}]
}
```

Each relation entry:
- **ref** (required): The composite key `{pubkey}.{id}` of the target object.
- **revision** (optional): Pin to a specific revision of the target. Default: latest.
- Additional fields (optional): `title`, `summary`, `url`, `instruction`, or any other hints to help agents understand the relation without fetching the target.

To parse a composite key: `split('.')` -> `[pubkey, id]`

## Self-Describing Objects

The `instruction` field is the core innovation. It tells any agent — human or LLM — how to interpret, display, and interact with the object. An object with a good instruction field can be understood without any external documentation.

For repeated types, use a `type_def` relation to reference a TYPE object that defines the schema and behavior for all objects of that type:

```json
"relations": { "type_def": [{"ref": "<pubkey>.<uuid>"}] }
```

Agents MUST read the referenced TYPE object's instruction before creating objects of that type. The type definition contains schema, required relations, and display guidance that the object's own `instruction` field alone may not cover. Skip only if the TYPE object cannot be found locally or on any reachable hub.

## Cryptography

- **Algorithm**: ECC P-256 (prime256v1) with ECDSA
- **Key format**: Compressed raw EC point (33 bytes), base64url encoded (44 characters, no padding)
- **Signing input**: Canonical JSON of `item` — compact, sorted keys (`jq -cS`), no trailing newline

### Public Key Format

The `pubkey` field contains the **compressed raw EC point** encoded as base64url (no padding):

- **Format**: `02|03 || X` (33 bytes) — EC point in compressed form
- **Encoding**: base64url (RFC 4648 section 5) — uses `-` and `_`, no `=` padding
- **Result**: 44 characters

### Generating a Keypair

```bash
# Generate private key
openssl ecparam -genkey -name prime256v1 -noout -out private.pem

# Extract compressed raw pubkey as base64url
openssl ec -in private.pem -pubout -conv_form compressed -outform DER 2>/dev/null \
  | tail -c 33 | base64 | tr '+/' '-_' | tr -d '='
```

### Signing

```bash
echo "$ITEM" | jq -cS '.' | tr -d '\n' > /tmp/item.json
openssl dgst -sha256 -sign private.pem /tmp/item.json | base64 -w0
```

### Verifying

P-256 compressed keys need a fixed 26-byte DER header for OpenSSL:

```bash
DER_HEADER="3039301306072a8648ce3d020106082a8648ce3d030107032200"
```

```bash
#!/bin/bash
# verify - Verify signature of an instructionGraph001 object
FILE="${1:?Usage: ./verify <file.json>}"
DER_HEADER="3039301306072a8648ce3d020106082a8648ce3d030107032200"

jq -cS '.item' "$FILE" | tr -d '\n' > /tmp/ig_item.json
jq -r '.signature' "$FILE" | base64 -d > /tmp/ig_sig.bin

PUBKEY_B64URL=$(jq -r '.item.pubkey' "$FILE")
PUBKEY_B64=$(echo "$PUBKEY_B64URL" | tr '_-' '/+')
case $(( ${#PUBKEY_B64} % 4 )) in
  2) PUBKEY_B64="${PUBKEY_B64}==" ;;
  3) PUBKEY_B64="${PUBKEY_B64}=" ;;
esac
PUBKEY_HEX=$(echo "$PUBKEY_B64" | base64 -d | xxd -p -c 100)

echo -n "${DER_HEADER}${PUBKEY_HEX}" | xxd -r -p > /tmp/ig_pub.der
openssl ec -pubin -inform DER -in /tmp/ig_pub.der -outform PEM -out /tmp/ig_pub.pem 2>/dev/null

openssl dgst -sha256 -verify /tmp/ig_pub.pem -signature /tmp/ig_sig.bin /tmp/ig_item.json
```

## Design Principles

- **Self-contained**: Every object carries enough context to be understood independently.
- **Transport agnostic**: Objects can travel via any channel — APIs, DHTs, Bluetooth mesh, LoRa, QR codes, steganography, blockchains. The format doesn't care how you got the data.
- **Graceful degradation**: No strict hash-chains that break when one object is missing. Signatures double as integrity checks. Related objects are nice-to-have, not hard dependencies.
- **Offline resilience**: The graph must function as well as possible while fragmented. When internet goes down, any previously usable app should still be usable — it's already local, along with whatever data it had cached. Apps that run on the graph tend to be *in* the graph (PAGE objects for browser apps, APPLICATION objects for apps that boot in an LLM), so they travel and cache alongside the data they operate on.
- **Composable**: Objects can belong to multiple realms simultaneously via the `in` array. The signer controls realm membership.

## Realm Membership (`in` field)

The `in` field is an array of realm strings inside `item` (signed). It declares which databases, networks, or communities the object belongs to. The signer controls this — realm membership is a deliberate, signed act.

Objects can belong to multiple realms: `["dataverse001", "acme_internal"]`. Realm semantics (propagation rules, access control, discovery) are defined by each realm, not by this format spec.

## Revisions and Conflict Resolution

One object per composite key. Updates increment `revision`. On sync conflicts, higher revision wins.

---

# dataverse001 — A Global Database on instructionGraph001

dataverse001 is a specific **realm** built on the instructionGraph001 data format. It's a truly global, decentralized graph where data objects can be scattered anywhere — files, disk slack space, steganography in images — and reassembled by any agent that discovers them.

Every object is a self-contained JSON fragment with `"dataverse001"` as a magic marker. When any device encounters this marker, it knows the object belongs to the global graph and can fold it into its local view.

## What Makes dataverse001 Special

Most realms are scoped — a corporate database, a private space, a project-specific store. dataverse001 is different: it's the **global commons**. Objects in this realm are meant to spread through every medium they can find and live forever.

Putting `"dataverse001"` in your `item.in` array is a deliberate, signed act. You're saying: "This should propagate globally."

### Wild Scanning

Scanners grep for `dataverse001` in raw byte streams as a pre-filter, then parse JSON and verify that `item.in` contains `"dataverse001"`. Objects can be discovered in any data stream — disk images, network captures, embedded in other files.

Other realms don't get this treatment. They propagate through configured routing, not through wild scanning.

## Realm Composition

dataverse001 is not a walled garden. It composes with other realms:

- **Cross-realm type sharing**: A custom realm references type definitions from dataverse001 via `type_def` relations. The type lives here, the object lives in the custom realm.
- **Multi-realm membership**: `["dataverse001", "acme_internal"]` — visible to both global and internal agents.
- **Server-public realm**: Use `"server-public"` for objects that should be readable by anyone on the current hub but should not propagate globally. This is useful for hub-local public apps, business data, or deployment-specific content.
- **Private realms**: Use a pubkey as realm name for a private space. Omit `"dataverse001"` to keep it off the global graph.

"Realm" rather than "namespace" — the intent is composition, not isolation.

## Well-Known Realm Semantics

The dataverse currently relies on a few realm patterns with distinct visibility and propagation behavior:

- **`dataverse001`** — readable by anyone and intended for global propagation and wild scanning.
- **`server-public`** — readable by anyone, but hub-local by default. Objects in this realm stay on the hub they are pushed to unless a specific deployment chooses to forward them upstream.
- **`<pubkey>` identity realm** — readable only by the matching authenticated identity.
- **Shared realms** — readable only by authenticated members configured by the hub.

Use `server-public` when you want public readability without turning something into part of the global commons.

## Graph Structure

Objects connect through named relations. There's a root node that structural objects link back to, but subgraphs can exist independently.

### Root Node

The root node (`00000000-0000-0000-0000-000000000000`) is the entry point. It links to core types, identities, the app directory, and other foundational structures.

Structural objects (identities, communities, specs, types) should include a `root` relation with a `url` for discoverability. Content objects (posts, comments) don't need this — they're reachable through structural objects.

### Agent Behavior

Any agent encountering verifiable dataverse001 objects should store them. The graph grows through opportunistic collection — objects propagate because agents find them useful enough to keep and share.

**Agents MUST NOT add objects to the `dataverse001` realm by default.** The `dataverse001` realm is the global commons — objects placed there propagate everywhere and are intended to be world-readable forever. Only add `"dataverse001"` to an object's `in` array when the user has explicitly stated that they want that specific data to be world-readable.

## Licensing & AI Training

Every object can optionally declare its license and usage permissions via `item.rights`:

```json
"rights": {
  "license": "CC0-1.0",
  "ai_training_allowed": true
}
```

- **`license`**: An [SPDX identifier](https://spdx.org/licenses/) (e.g. `"CC0-1.0"`, `"CC-BY-4.0"`, `"CC-BY-SA-4.0"`). Declares the license for this object's content.
- **`ai_training_allowed`**: `true` or `false`. Explicit signal whether the content may be used for training AI models.

If `rights` is absent or a field within it is absent, no assumption is made — the author simply hasn't declared.

**Agents MUST NOT include a `rights` section by default.** Only add `rights` when the user has explicitly stated their licensing preferences.

### Data by the People, for the People

Much of the dataverse's structural foundation — types, schemas, protocols, recipes — is public domain (`CC0-1.0`) and explicitly available for AI training. Not just by a few large corporations, but by anyone.

We encourage contributors to share under `CC0-1.0` with `"ai_training_allowed": true`. But it's your data — use whatever license fits your intent.


