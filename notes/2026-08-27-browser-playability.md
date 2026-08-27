---
date: 2026-08-27
status: draft design — no browser durable-write path is implemented
basis: gitseq-chess main at 0ccbeb76f0819976abd155071c6cf81d1b5146c3
---

# Browser playability without server-held player keys

## Decision

The browser's Ed25519 key is the chess actor. It may take a seat and sign its
own durable joins and moves. The serve process relays those signed acts to the
log, but never receives, stores, or uses a player private key.

Do not delegate browser play to a server-held player key. Delegation would make
the process able to forge every move made through it, turn cross-site request
defences into the only thing separating a hostile page from a player signature,
and create another custody system beside the command-line one. None of that is
needed: the browser already proves possession of an Ed25519 key and signs live
chat locally.

This decision makes the live and durable identities one key. A lease still
grants only process-local presence. The same key's durable signature, not the
lease and not the HTTP connection, says who attempted a chess act.

## Current boundary

The current page generates a non-extractable key into a module variable when a
person joins the live room. Reload loses it. `newReadHandler` opens projections
only; the HTTP service has no `host.Workspace` durable writer. The command line
is the only surface that calls `Workspace.Append` through chess helpers such as
`Join` and `Move`.

There is also a public-host API gap. `Workspace.Append` accepts a private key
and constructs the kernel intent internally. The canonical intent and kernel
submission types are private to the Gitseq module, so this outside application
cannot correctly relay a signature created elsewhere. Implementation therefore
starts with a small Gitseq public-host addition that prepares canonical signing
bytes and submits the matching public key and signature. It must not expose
kernel internals or application meaning.

## 1. Identity, persistence, and loss

Generate one Ed25519 key pair per chess repository and browser origin with
WebCrypto using `extractable: false`. Store both `CryptoKey` objects in one
IndexedDB record keyed by the repository genesis and a local format version.
IndexedDB's structured-clone storage preserves a non-extractable private
`CryptoKey`: reload can use it for `sign`, but JavaScript cannot export its key
bytes. Store the raw public key beside it for display and requests. Complete
that transaction before offering a durable join.

The repository genesis, not a path or game identifier, partitions keys. The
browser origin remains an additional custody boundary. Changing browser
profiles, clearing site data, or serving the same repository from a different
origin loses access to the key. The UI must say this before an unanchored key
takes a seat and must never silently replace a missing key while presenting the
new fingerprint as the old player.

Non-extractable means there is no private-key backup or import/export feature.
Recovery uses the host identity rules already folded by chess:

- A seat anchored to a persistent identity at create or join can be used by a
  replacement browser or CLI key that is currently anchored to the same
  identity and `chess` or `chess:<game>` scope.
- An unanchored seat initially belongs to its exact key. It may be upgraded
  later, but the original key must still be present and make an effective
  chess act after it is anchored; that act commits the upgrade. Merely
  anchoring a replacement after the original key is gone does not recover the
  seat.
- Therefore loss of an unanchored key before such an upgrade is final: no
  remaining key can act for that seat, and the game may be unable to finish.
  The UI must state that consequence. The design does not hide it behind a
  server recovery key.

An existing anchored CLI or browser key can endorse a new browser fingerprint
through the current host identity vocabulary. A persistent Nostr root can also
anchor a new subject with its existing proof. Adding a convenient browser
identity ceremony may be separate work; it does not require new seat or fold
semantics.

## 2. Browser-signed submission

Use one two-step relay for a finite allowlist of browser acts. The first
implementation needs only `join` and `move`; creation, naming, resignation,
and draw actions can remain on the CLI until separately requested.

1. The browser opens or renews its existing possession-proved live session
   with the persisted public key. The lease is useful for request scoping and
   rate bounds, but is not durable authority.
2. The browser sends a bounded JSON prepare request containing the game, the
   requested join or UCI move, its public key, its credential, and a fresh
   idempotency key retained until the outcome is known.
3. The server checks that the credential is bound to the same game and public
   key. It reads one verified projection, rejects an obviously inapplicable
   request, and uses a pure chess act builder shared with the CLI to produce
   the canonical payload and causal reference. It asks the public host API for
   the exact signing bytes. The response echoes the human-readable action and
   the opaque prepared act so the page can confirm it still matches the click
   before calling WebCrypto.
4. The browser signs those bytes with its persisted private `CryptoKey` and
   returns the prepared act, public key, and signature. The private key never
   crosses the WebCrypto boundary.
5. The server rechecks all bounds, the credential-to-key binding, the allowed
   schema, and the prepared act's application fields. The public host API
   recomputes the local target, payload-tree identity, namespace, and exact
   intent; verifies that the submitted signature covers that intent; and gives
   it to the repository's sequencer. The server then folds the returned record
   and reports its record identifier and effective/refused decision exactly as
   the CLI does.

The serve process holds and uses the repository's sequencer key because it
orders admitted records. It never holds or uses a player private key. A
sequencer signature cannot stand in for the browser actor signature.

The same race semantics as the CLI remain visible. Another append may advance
the accepted move chain after preparation. The signed act still names the
chain the person saw; if it becomes stale, the fold may retain it as an
ineffective attempt. The idempotency key makes retrying the same prepared act
one act, not a second attempt.

### Hostile-page defence

A page on another origin cannot read or invoke the origin-scoped IndexedDB
`CryptoKey`. Durable prepare and submit routes stay behind the existing trusted
host, JSON content-type, `Origin`, and `Sec-Fetch-Site` checks, with no CORS
grant. Every submit also needs a valid signature from the public key bound to
the session. A cross-origin page can therefore send, at most, a request that is
refused; it cannot ask the server to sign because the server has no player key.

The browser is nevertheless untrusted server input, even when same-origin.
The server does not trust its role, legal-move claim, payload, causal reference,
public-key fingerprint, idempotency fields, or signature. It derives or
validates each one and leaves final authority to signature verification and
the fold. Requests and responses remain bounded.

Non-extractability is custody, not protection from same-origin script. Script
running in this origin can ask the private `CryptoKey` to sign. The existing
self-contained assets, restrictive Content Security Policy, absence of
third-party script, escaped templates, and no-store responses are therefore
part of the player-key boundary. An actual same-origin script compromise has
the authority of that browser actor until the key or its identity anchor is
revoked; pretending otherwise would overstate the design.

## 3. Exact move intent bytes and ownership

For a move, the chess application owns these application bytes and fields:

- schema: `chess/move@0`;
- payload: the UTF-8 bytes of canonical JSON
  `{"game":"<game event id>","move":"<lowercase UCI>"}` with the `game`
  field first, no extra whitespace, and the same escaping as Go's
  `json.Marshal(MovePayload{...})`;
- `rests_on`: a one-element array containing the projection's current accepted
  `LastMove` record.

The Gitseq host owns the bytes signed by Ed25519. For the current protocol they
are:

```text
UTF8("gitseq.intent.v0\0") || CoreDetCBOR([
  0,                              // intent version
  "git:<object-format>:<genesis>",
  "chess/move@0",
  0,                              // envelope version
  "git:<object-format>:<payload-tree>",
  ["<current accepted LastMove>"],
  "chess",                        // idempotency namespace
  "<idempotency key>",
  null                            // current zero capability hash
])
```

`payload-tree` is the ordinary Git tree identity of the exact payload and no
attachments, computed for the repository's object format. The browser does
not reimplement canonical JSON, Git object hashing, or deterministic CBOR. It
base64-decodes and signs the exact byte string returned by the host-backed
prepare operation. That leaves one encoder for each layer: chess Go code for
application payloads and Gitseq host code for kernel intents.

The host API must make the prepared structure opaque to JavaScript while still
returning the action echo used for the person's confirmation. Submission must
compare the opaque structure with a fresh recomputation before any object or
record is written. Changing the echoed move while reusing a signature must be
refused before append.

### Browser/server parity gate

Pin one fixture with a repository target, canonical move payload, predecessor,
idempotency key, Ed25519 test key, exact signing bytes, and signature. A Go test
produces the fixture through the public host prepare API. A browser JavaScript
test base64-decodes those bytes and signs them with WebCrypto; the Go submit
path must accept that signature and recover the same actor, schema, payload,
and causal reference. Tampering with the game, move, predecessor, public key,
or any signing byte must fail before append.

Because the browser has no intent encoder, browser/server encoding divergence
is structurally absent. The fixture is still required: it catches transport,
base64, domain-prefix, WebCrypto, and future protocol changes. If a browser
encoder is ever introduced, the same gate must compare its byte-for-byte output
with the Go host encoder before it can replace this design.

## 4. Seat binding and CLI interoperability

Joining in the UI is a normal `chess/join@0` act signed by the persisted browser
key and resting on the create record. `SeatFor` then recognizes the browser
fingerprint as the seat holder. The UI may use the URL-fragment invitation
secret already kept out of referrers and include it only in the signed join
payload. For an exact-key invitation, the creator must invite the browser
fingerprint shown by the UI; a different CLI key cannot substitute for it.

Keys interoperate through existing seat semantics, not through import or
server impersonation:

- A CLI-created or CLI-joined unanchored seat can be played only by that exact
  CLI key. A browser key is a different actor and cannot borrow it.
- A browser-joined unanchored seat can be played only by that browser key. Its
  non-extractable private half is not copied into a CLI key file.
- If the seat was anchored when taken, or was later upgraded by an effective
  act from its original key, any currently anchored browser or CLI key for the
  same persistent identity and chess scope can play it.

Thus CLI/browser interoperability is available where the repository already
has a persistent identity relationship. It is not inferred from a display
name, possession of the same machine, or access to the same HTTP service.

## 5. Scope boundary

This design does not change the chess fold, schemas, seat matching, legal-move
rules, or durable decision rules. It does not weaken or replace the loopback
listen restriction or `TrustedHostHandler`; the new write routes use that
boundary unchanged. It does not turn live presence, motion, chat, a role
preview, or a lease credential into durable authority.

It also does not add key export, cloud key escrow, a server-held player or
recovery key, a second chess rules engine in JavaScript, or a browser copy of
the kernel intent encoder. Browser game creation and the remaining durable
actions are follow-up surface work, not reasons to make the first join-and-move
path generic beyond its finite schemas.

## Conclusions

**Architecture.** This note changes no contract. Implementation affects the
Gitseq application-host public surface, the chess resident composition, and the
browser UI. The kernel protocol and chess application fold remain unchanged.
Because externally prepared and submitted signatures extend the exported
`host` contract, that Gitseq change needs its own exact-head architecture
update and review before the chess repository consumes it. Chess should factor
pure join/move act builders so CLI and HTTP share application encoding.

**Security.** The trust boundary moves from a memory-only browser actor to an
origin-persisted browser actor, not into the server. The server remains an
untrusted-input verifier and sequencer; it cannot forge a player act. The main
risks are same-origin script use of the non-extractable key, loss of unanchored
key custody, cross-origin or DNS-rebinding requests, malformed or oversized
signed submissions, replay, and stale move races. The design addresses them
with self-contained same-origin code and CSP, explicit loss semantics, the
unchanged loopback/trusted-host mutation guard, bounded canonical verification,
signed idempotency, and fold-visible race outcomes. A same-origin script
compromise remains player-key compromise and must be described as such.

**Simplification.** Use one browser key for live proof and durable chess acts,
one canonical Go application encoder, one canonical host intent encoder, and
one prepare/sign/submit relay. Reuse the current fold, identity recovery,
`SeatFor`, idempotency, and decision reporting. Do not add delegation to a
server key, private-key export, another recovery ontology, or duplicated
browser encoding.
