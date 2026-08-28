---
date: 2026-08-27
status: identity front door implemented; durable chess writes remain a design
basis: Gitseq public host identity prepare/submit API at 7152e79a741e6c9c277568a6aabec8e9b6cbd792
---

# Browser playability without server-held player keys

## Decision and current scope

The browser's non-exportable Ed25519 key is the browser actor. The serve
process may relay acts signed by that key to the log, but it never receives,
stores, or uses the private half. The first durable browser surface is identity:
GitHub and Nostr can anchor the tab key, and an anchored tab key can endorse an
agent key within a bounded chess scope and expiry. Durable browser joins and
moves remain outside the current implementation.

Do not delegate browser play to a server-held player key. Delegation would make
the process able to forge every move made through it, turn cross-site request
defences into the only thing separating a hostile page from a player signature,
and create another custody system beside the command-line one. None of that is
needed: the browser already proves possession of an Ed25519 key and signs live
chat locally.

This decision makes live presence and identity endorsement use one tab key. A
lease still grants only process-local presence. A durable identity act is
authorized by the key's exact signature, not by the lease or HTTP connection.

## Current boundary

The page generates a non-exportable key into a module variable. Reload or tab
closure loses it. The landed public-host API now lets an outside application
call `Workspace.Prepare`, obtain `ActorSigningBytes`, and submit a matching
`SignedAct` through `Workspace.AppendSigned`. Gitseq still owns canonical intent
encoding and verification; chess does not expose kernel internals or reproduce
that encoding in JavaScript.

## 1. Identity, persistence, and loss

Generate one Ed25519 key pair per tab with WebCrypto using
`extractable: false`. Keep both `CryptoKey` objects in memory. The raw public
key is sent to the service and its fingerprint is displayed. The UI keeps no
key or prior fingerprint in browser storage. It stores no private key, token,
live credential, bearer, or root-key material.

Changing tabs, reloading, closing the page, changing browser profiles,
clearing site data, or serving the repository from another origin loses the
private key. A fresh session generates a visibly different fingerprint. The
UI never presents that new key as the old player. Recovery uses only the host
identity rules already folded by chess:

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
through the current host identity vocabulary. A persistent GitHub or Nostr
identity can also anchor a new session key through the browser ceremony. This
adds no seat or fold semantics.

## 2. Browser-signed identity submission

Use a two-step relay for the finite identity-anchor schema. Joining, moving,
creation, naming, resignation, and draw actions remain on the CLI or MCP until
separately implemented.

1. The browser may open or renew its existing possession-proved live session
   with the current tab public key. The lease is useful for live-room scoping and
   rate bounds, but is not durable authority.
2. For Nostr, the browser asks the server for the exact NIP-01 event template
   whose content is `identity.NostrDelegation`. It passes that event object
   directly to `window.nostr.signEvent` without serializing or reconstructing
   it. GitHub instead uses one-shot same-origin OAuth state and PKCE; the
   provider token exists only during the server-side lookup and witnessing act.
3. For a self-signed Nostr anchor or an agent-key endorsement, the browser asks
   the server to prepare the bounded host identity act. The response contains
   an opaque draft and the exact host signing bytes.
4. The browser signs those bytes with its in-memory private `CryptoKey` and
   returns the prepared act, public key, and signature. The private key never
   crosses the WebCrypto boundary.
5. The server rechecks all bounds, the allowed schema, the subject, exact
   `chess` or `chess:<game>` scope, expiry, and cached prepared draft. The
   public host API verifies the submitted signature and gives the act to the
   repository sequencer. The server then folds the returned record and reports
   whether the anchor is actually in force; append success alone does not
   imply identity authority.

The serve process holds and uses the repository's sequencer key because it
orders admitted records. It never holds or uses a player private key. A
sequencer signature cannot stand in for the browser actor signature.

Prepared identity drafts are bounded, process-local, one-shot, and expire. The
server accepts only the opaque draft identifier it issued and recomputes the
actor from the submitted public key. A durable endorsement can still be
recorded but ineffective; the folded identity outcome, not HTTP success or
append success, determines what the UI may call anchored.

### Hostile-page defence

A page on another origin cannot read or invoke the tab's in-memory `CryptoKey`.
Durable prepare and submit routes stay behind the existing trusted
host, JSON content-type, `Origin`, and `Sec-Fetch-Site` checks, with no CORS
grant. Every submit also needs a valid signature from the public key bound to
the prepared draft. A cross-origin page can therefore send, at most, a request that is
refused; it cannot ask the server to sign because the server has no player key.

The browser is nevertheless untrusted server input, even when same-origin.
The server does not trust its identity claim, subject, scope, expiry, Nostr
event, public-key fingerprint, draft, or signature. It derives or validates
each one and leaves final authority to signature verification and the fold.
Requests and responses remain bounded.

Non-extractability is custody, not protection from same-origin script. Script
running in this origin can ask the private `CryptoKey` to sign. The existing
self-contained assets, restrictive Content Security Policy, absence of
third-party script, escaped templates, and no-store responses are therefore
part of the player-key boundary. An actual same-origin script compromise has
the authority of that browser actor until the key or its identity anchor is
revoked; pretending otherwise would overstate the design.

## 3. Future durable move intent bytes and ownership

The following remains the design for a later browser move surface; the current
identity front door neither prepares nor submits `chess/move@0`.

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

## 4. Future browser seat binding and CLI interoperability

If browser joining is implemented, it will be a normal `chess/join@0` act
signed by the tab key and resting on the create record. `SeatFor` would then
recognize the browser fingerprint as the seat holder. Such a UI may use the URL-fragment invitation
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
the kernel intent encoder. Browser game creation and durable chess actions are
follow-up surface work, not reasons to make the identity path generic beyond
its finite host schema.

## Conclusions

**Architecture.** This chess implementation consumes the already landed Gitseq
application-host public surface and changes the chess resident composition and
browser UI. It does not change the Gitseq host contract, kernel protocol, or
chess application fold. A future browser move path should factor pure
join/move act builders so CLI and HTTP share application encoding.

**Security.** The trust boundary remains a memory-only browser actor; it does
not move into the server. The server remains an untrusted-input verifier and
sequencer and cannot forge a browser act. The main risks are same-origin script
use of the non-exportable key, loss of unanchored key custody, cross-origin or
DNS-rebinding requests, malformed or oversized signed submissions, replay,
provider-secret disclosure, and stale-act races. The implementation addresses
them with self-contained same-origin code and CSP, explicit loss semantics, the
loopback/trusted-host mutation guard, one-shot OAuth state and PKCE, fixed
provider endpoints, transient tokens, bounded canonical verification, and
fold-visible outcomes. A same-origin script compromise remains browser-key
compromise and must be described as such.

**Simplification.** Use one browser key for live proof and durable identity
acts, one canonical host intent encoder, and one prepare/sign/submit relay.
Reuse the current fold, identity recovery, `SeatFor`, and decision reporting.
Do not add delegation to a server key, private-key export, another recovery
ontology, or duplicated browser encoding.
