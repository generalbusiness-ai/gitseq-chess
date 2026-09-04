# Browser play and forge-confirmed local deployment

Proposed decision under native design child `2e3bc7b7` of item 6 request
`025a6e01`. This reconciles the adopted second-application design with identity
delivery at `1abf379f21df58da3fcc3277e72a14197db56104` (reviewed candidate
`c191466171e67dbef4298edbfdaaa83bd5960efd`). Public deployment remains gated.

The current original-design artifact is Gitseq `71c4947b`, at source
`76644a9639a40acf0606a8da51743a3342d619cf`. Native request `967e62b6`
(sequence 276) commissioned the earlier browser design and explicitly requires
Hugh's adoption before implementation. Its satisfied design/review commitment
is not adoption of this deployment delta.

## Decision to make

Deliver browser join and move signing, then a forge-backed container recipe
that remains reachable only through the existing loopback HTTP boundary.
Use the current memory-only tab key. Preserve the original obligation to make
a public service, but keep public exposure and multiple-host failover gated on
an explicit supported-host and writer-custody decision. A local container is
not evidence that the public-service obligation has been fulfilled.

The original design proposed an IndexedDB key and a public submit endpoint.
The reviewed identity implementation deliberately keeps the key only in the
tab, and its mutation guard accepts loopback hosts and same-origin HTTP.
Persisting the key or placing an internet proxy in front would change those
contracts. This proposal adopts the memory-only key for browser game actions
and retains the public-deployment gate rather than silently changing either.

## What already exists

| Layer | Existing behavior | Work still needed |
| --- | --- | --- |
| Gitseq public host | `Workspace.Prepare`, `ActorSigningBytes`, `AppendSigned`, `OpenAttached`, verified `Records`; the pinned dependency is `7152e79a741e` | No new intent encoder or private host API |
| Chess application | Pure fold, invitations, scoped anchored seats, legal moves, durable decisions; CLI and MCP use the same application functions | Factor pure join/move act builders out of the local-signing wrappers |
| Browser | Embedded board, legal-destination query, presence/chat and memory-only Ed25519 key | Join/move confirmation and prepare/sign/submit interaction |
| Identity | GitHub possession challenge, OAuth/PKCE and external witness signing; Nostr and bounded agent endorsement | Route identity writes and reads through the same forge-confirmation boundary when that mode is selected |
| Service | Loopback listener, trusted-host and same-origin mutation checks, bounded requests | One writer ownership guard, pending-push state, confirmed projection and container recipe |

Identity append success currently means local durable storage. Forge mode
must not report an anchor as connected before its record reaches the forge.
The same rule applies to moves, read APIs, board updates and live role labels.

## Browser actions and custody

Keep one non-exportable Ed25519 key in the existing browser module. No
IndexedDB, local/session storage, key export, server player key or recovery
secret is added. Reload or tab closure loses the key. Warn an unanchored
player before joining; losing that key can permanently strand the seat.
An anchored player may create a new tab key, anchor it to the same persistent
identity and qualifying scope, then recover the seat through the existing fold.
Anchoring a replacement cannot recover a lost unanchored seat. Display names,
presence credentials and successful HTTP responses grant no seat authority.

Factor application functions that construct a `host.Act` from validated
application inputs. Join rests on the create event. Move takes an explicit
accepted `LastMove` from a verified projection and rests on that exact event.
Existing CLI/MCP wrappers retain their behavior and call those builders before
local signing. The browser relay calls the same builders before host preparation.
The fold remains the sole judge of seats, legality and causal currency.

The person chooses a join or a UCI move. The server returns an opaque draft
identifier, an action echo, the actor fingerprint and exact host signing bytes.
The browser checks the echo against the chosen action and signs those bytes.
It does not encode a kernel intent, hash Git objects or implement chess rules.
Submit contains only the draft identifier, public key and signature. The server
binds it to the cached action, actor, repository, game, predecessor and expiry,
then uses `AppendSigned`. Altered bytes or actor fail before any append.

Use a finite join/move intake, an 8 KiB application payload ceiling and an
explicit bounded request envelope. Drafts expire after five minutes with at
most 128 pending entries, matching the existing identity draft policy.
Saturation refuses immediately; a live session is not required as authority.
Extend the existing mutation-path guard to these exact routes. Keep JSON-only
POSTs, loopback host validation, Origin/Sec-Fetch-Site checks, no CORS, existing
CSP, bounded read/write timeouts and no third-party browser scripts.

Stable retry identity belongs to the prepared act. During the bounded draft
lifetime, a lost-response retry uses the same signature and cached intent;
it never prepares a changed predecessor under the old retry key. Retain bounded
receipt state for those exact retries. An expired draft or service restart
refuses an unknown draft. The browser refreshes the confirmed board and asks
for a new explicit action if still needed. Do not invent a durable draft store
or reconstruct missing draft authority merely to promise transparent retries.

A stale move may be durably recorded and refused by the fold. Report the
record ID, confirmed frontier, effective/refused result and reason. A join
race leaves one occupied seat and an explicit refusal for the other player.
The UI renders the confirmed fold and snaps a refused move back. Preparing
an action never reserves a seat or promises that it will remain legal.

Initial game creation and invitation generation remain available through
the delivered CLI/MCP. The browser follows the existing invitation fragment;
the secret appears only in the signed join payload, never in an HTTP query,
referrer or access log. Browser creation and the remaining act controls must
be assigned explicitly if required for the adopted onboarding acceptance.

## One writer and a confirmed frontier

The first supported forge mode has one repository checkout on one POSIX host,
with all writer processes sharing its real Git common directory. Hold an
exclusive OS lock for the writer process lifetime. Every supported writer
entry point must obey that lock; local CLI/MCP cannot append around a running
forge service. A second process refuses writes. Losing ownership stops intake.
This lock is not a distributed lease and does not support independent clones
or multi-host rolling deployment. Those require their own adopted lease and
fencing contract before such a recipe can be advertised.

Configure one forge remote, exact sequence ref and expected genesis. Startup
fetches and verifies the forge sequence and binding before opening attached
sequencer custody. Refuse a divergent local branch, mismatched genesis,
unexpected binding or unavailable ownership guard. Never initialize a new
sequence as an implicit recovery action. The forge ref is the authority in
this mode; a local append alone is pending delivery.

Serialize append and push. Once an act is locally appended, freeze further
durable intake until its exact sequence advance is confirmed on the configured
forge ref. Push fast-forward only. Publish success and advance the served
projection only after confirmation; a successfully pushed but ineffective act
still reports the fold's refusal. Serve all board, identity and role reads from
the last forge-confirmed verified prefix. Never let a separate handler reopen
the pending local head and leak it as confirmed state.

On timeout, the push may have succeeded. Inspect the exact forge ref and verify
whether it contains the pending record before returning success. Otherwise
show pending/unavailable and retry that exact advance with bounded backoff.
A rejected divergent push stops writes and requires operator reconciliation.
Do not force-push, rebase records, invent replacement signatures or announce
the pending move as accepted. Restart reconciles local pending history against
the verified forge before accepting another write. An irreconcilable branch
is retained as evidence; it is not silently deleted or replayed as new acts.

The implementation must make the confirmed-prefix reader shared by every
service surface, including GitHub callback completion, Nostr endorsement,
identity status and live seat/role views. Forge mode cannot be added solely
as a wrapper around the move HTTP response.

## Supported recipe and acceptance

Propose a pinned build and a Linux container recipe using host networking and
an explicit `127.0.0.1` listener. Browsers run on that same host. Mount the
repository and ownership lock on one shared local filesystem; mount sequencer
and forge credentials privately at runtime. Do not bake keys into the image,
publish a container port on all interfaces or rewrite Host/Origin headers.
Native local play remains supported independently of this Linux recipe.
Do not claim this recipe works on macOS/Windows container networking without
running and documenting a boundary-preserving variant.

Count the sequencer key and forge push credential as two secrets. GitHub login
additionally needs its provider registration/client secret and the existing
external witness signer with a durably declared public key. The chess server
receives witness signatures, not the witness private key. A container recipe
must retain that existing boundary rather than importing the witness key into
the game process. Use isolated test identities for acceptance.

Before approval, execute these gates at the exact candidate:

- Go/WebCrypto fixtures sign host-provided bytes; Go accepts them as the same
  actor/action. Mutating the echo, game, predecessor, signature or public key
  fails the appropriate confirmation or append gate.
- Join races, stale moves and exact retries preserve fold results and record
  counts. Planted omission of signature, predecessor or draft bounds fails.
- A second writer and a writer without the shared lock cannot append. Crash
  releases ownership; restart refuses unresolved divergent history.
- Simulated rejected push, outage, lost response after successful push and
  crash between append/push preserve the confirmed board and identity view.
  Planted early acknowledgment or pending-head read must fail tests.
- Two actual browsers join/play through the adopted container boundary and
  forge. Recover an anchored seat after clearing browser state; refuse wrong,
  unanchored, expired and revoked identities. Show truthful anonymous key loss.
  Record environment, source heads, commands, counts and durable checkmate.

The native item-8 request `78742cbf` owns the end-to-end evidence. Its CLI/MCP
variant can run independently. Its browser/container variant waits for this
delivery and for explicit adoption of the tested boundary; a local-container
run cannot stand in for an unapproved public-host claim.

## Delivery and remaining authority

Obtain Hugh's explicit adoption of the memory-only key reconciliation,
local-container scope and public-deployment gate through a proposal resting on
this exact design artifact. Then request independent architecture, security
and simplification review, resting on the ratified proposal. Before any planning-stage
closure, file concrete implementation successor requests resting on that
adoption and this exact design artifact, and link item-8 acceptance to them.
Keep parent `025a6e01` in flight until its implementation and any explicitly
deferred destination are accounted for. Implementation cannot start on this
draft's wording alone.

Architecture: application payload construction stays in Chess; intent encoding
and actor verification stay in the public Gitseq host; sequencing remains the
kernel's job. The new service contract is forge-confirmed delivery and reads.
Its implementation must update the repository architecture documentation and
publish the exact candidate artifact for that contract.

Security: the important boundaries are tab-key custody, untrusted signed
intake, same-origin HTTP, sequencer/witness separation, shared writer ownership
and the forge ref. Distributed ownership and public exposure remain unresolved
outside the proposed local recipe and require explicit decisions.

Simplification: reuse the existing fold, shared pure act builders and host
signing bytes. One serialized delivery path and one confirmed reader serve
both game and identity actions. No browser rules engine, second identity
system, new key store or generic public submission gateway is required.
