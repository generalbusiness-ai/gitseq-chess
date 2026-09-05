---
date: 2026-09-05
status: proposed decision; public implementation and activation not authorized
request: git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:285b3f419115eafb15522601227f13f881742fe8
rests_on:
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:f875e7ae6c59ec35e457dc8fc84e71320d44112f
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:e1772baef05e167dc4fc00f63f5d48da5642aec8
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:51c59ad5fe04279021466136d4831ef886f15296
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:212f9f39f5aa1d5e843924260f1f607a9e531c91
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:d8ef5ff816e2254719995a29e2140de49ce770d5
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:e7eca3ce5349b9e5a3ce1a75dafded656bd81217
  - git:sha1:1b96687964cbba5a6089c526d7136b32ba9488a5#git:sha1:f74f93a0efa0c8f102808243da2aeeb01d008fa8
---

# Proposed first public Chess host

A bounded design child of native item 6. Adoption selects the deployment
contract below; it does not authorize public implementation, provisioning or
activation. The parent public-service promise remains open.

The adopted local decision is `notes/2026-09-04-browser-delivery.md` at
`e7a911827bc96e7bc56ee2ddd6eb7fa72d423d4e`, proposal `272a84c7`, ratification
`523d0864`. Local implementation and browser acceptance are delivered at
`e7a65612a8270b12f676a35e97c66aaf2a705a30`. That decision deliberately leaves
public exposure unresolved. It cannot authorize a public reverse proxy merely
because the backend still binds loopback.

## Recommendation

Use one operator-owned Linux host, one local POSIX checkout and the existing
single writer. Put one TLS terminator on that same host and keep Chess bound
to loopback. Serve one explicitly configured HTTPS origin, with no tenant or
origin wildcards. Preserve the browser's Host and Origin; do not disguise public
requests as localhost. Keep sequencer, forge and external witness custody as
already designed. No independent writer clone, distributed lease, automatic
failover or new player identity system is needed for this first destination.

This recommendation selects the deployment shape. Hugh still must name the
actual host/account, HTTPS origin and custodians before any deployment action.
No cloud vendor, machine, domain or credential has been selected or purchased.

## Smallest source contract change

The blocker is in Chess's own layer-7 HTTP composition, not a missing Gitseq
intent or identity API. At the delivered source:

- `cmd/chess/main.go:runServe` permits only a loopback listener. Keep that rule.
- `cmd/chess/web.go:trustedLiveHost` requires mutation Host to resolve only to
  loopback. `guardLiveMutation` accepts only an HTTP Origin matching that Host.
  A public HTTPS request correctly refuses today.
- `cmd/chess/web_identity.go:githubEndpoints` additionally requires a loopback
  redirect. Its callback guard already binds the configured host and path.

Add one explicit public-origin configuration to the existing HTTP policy,
validated at startup as a single HTTPS origin with no userinfo, path, query or
fragment. Local mode keeps its current behavior. In public mode all routes
require that exact Host authority; mutation routes compare a supplied Origin to
the configured exact origin. Reject repeated or conflicting Origin values,
including a forged first value followed by another header. Use one policy for
game, live and identity mutation routes and callback configuration. Reject
malformed, null or cross-site origins; preserve JSON-only bounded POSTs,
query restrictions, CSP and existing signatures/possession challenges. Signed
HTTP clients without browser Origin still need the exact configured Host and
all existing signature and bounded-input checks. Origin is a browser defense,
not player identity or permission to append.

The TLS terminator must preserve Host, reject unexpected names, strip incoming
forwarding headers and have no route to the backend except local transport.
Chess must not infer authority from arbitrary Forwarded/X-Forwarded-* headers.
The operator owns both processes; this is not a shared-host authentication
boundary. A remote or multi-hop proxy needs a separately specified trust path.

Leave GitHub disabled for the first pilot unless the operator chooses it.
GitHub anchoring, if enabled, uses the exact public origin plus the existing
callback path, possession challenge, state/PKCE and external witness. Provider
registration and witness configuration are operator inputs. Existing Nostr and
agent anchoring reuse the same host APIs and authority fold. Do not copy or
replace identity resolution, kernel signing bytes or the chess act builders.

## Resource admission and measurement

Keep admission in Chess's layer-7 composition and the one-host proxy, with one
small configuration record. Do not introduce a gateway framework, distributed
limiter, per-user billing or a second identity authority. Public startup must
refuse an absent, zero or unlimited budget. Enforce global ceilings before
expensive repository/signature/provider work, so fresh actor keys cannot reset
an allowance. Reject overflow with a bounded retryable response and no append.
Admission is not seat authority; add no public arbitrary-schema submit API.

The minimum record has finite active-read and active-mutation slots, the
existing separate long-poll slots, global request and durable-append rates with
finite bursts, and a hard writable-volume reserve/ceiling. Use immediate
admission or refusal rather than another unbounded queue. Count identity
mutations with game appends; disabled GitHub creates no provider-work allowance.
A restart must not reset the persistent storage ceiling. Protect the log and
its pending append when full; retain bounded reads and an operator recovery
path instead of deleting signed history to make room.

Focused current tests check real writer exclusion and HTTP boundary
refusal on e7a65612. Source bounds are 32 KiB request bodies, 8 KiB prepared game
payloads, 128 five-minute game drafts, 128 pending identity proofs and 32 live
waiters. These are not throughput or public-host capacity measurements. A
saturated draft count does not bound cumulative Git growth.

No named Linux destination exists yet, so an honest production request rate,
RAM requirement or disk allocation cannot be claimed. The implementation must
measure the chosen host before activation: idle footprint; one human and their
agent playing a full game through the actual TLS/forge path; p95 ordinary reads,
prepare, confirmed append and restart; peak RSS and disk bytes per signed act;
then saturation with repeated requests and changing actor keys. Pick the
smallest finite limits that pass that journey with recorded headroom, retain
32 as the waiter ceiling unless separately justified, and record values,
intervals, source/image and measurements in the destination record. Remove
each guard separately to show the refusal tests depend on it. Admission shape
can be adopted now; the measured values are an explicit activation gate.

## One human and their own agent

The intended first journey is: open the named service, recover or create the
human's supported identity, connect their existing agent once, choose a side,
and play with automatic replies and clear turn, retry and result messages.
GitHub registration is not a prerequisite; preserve the existing Nostr and
agent identity paths and their actual custody requirements.

The local playability review is still owed. Two-browser item-8 acceptance is
not evidence of the human-versus-agent journey. Source inspection
also exposes an integration boundary that the review must exercise: `serve`
holds writer ownership for its lifetime and local CLI/MCP writes acquire that
same ownership. An agent cannot be instructed to bypass that lock or use an
independent writer clone. Its supported client must submit signed choices
through the owning service. If the current CLI/MCP cannot do so, that is a
concrete client-transport gap for the local review and a separately scoped
repair, not a missing Gitseq identity API or permission to add a second writer.

Before public activation demonstrate that actual supported agent connection,
not a script impersonating both browser players: one full game, refresh with
supported anchored-seat recovery, agent disconnect/restart without an extra
move, ambiguous reply retried exactly once, clear result and rematch. Record
setup commands, copying and context switches. Incorporate the review's observed
friction into acceptance before commissioning the public implementation; do
not represent untested UI convenience as delivered.

## Destination record Hugh must supply or approve

| Field | Required concrete value |
| --- | --- |
| Service owner | Operator and supported Linux host/account; who can stop and recover it |
| Public origin | Exact HTTPS scheme/host/port, DNS and certificate-renewal owner |
| Game authority | Existing expected genesis, sequence ref, one forge fetch/push destination and remote owner |
| Custody | Runtime paths/owners for the existing sequencer and forge credentials; transfer procedure if this is a different host |
| Identity | Whether GitHub is enabled initially; if so, provider-registration owner, exact callback and external witness owner/public declaration |
| Operating budget | Measured admission limits, disk/log ceiling, retention and incident owner; no unbounded public pilot |
| Activation | Reviewed source/image digest, host backup/recovery evidence, rollback procedure and explicit permission for this named destination |

Private keys and tokens are not values to put in the workroom or this table.
A different machine does not inherit sequencer authority merely by cloning.

## Acceptance and sequence

Deliver this note through child #721 only, with normal adoption and independent
review. Its source merge does not close or transfer item 6's parent promise.

Then commission the smallest separate implementation sequence:

1. Reconcile the local human-versus-agent review and any required client
   transport repair. This can proceed without selecting a public host.
2. Add the explicit origin policy and its proxy fixture, then finite admission
   with measured saturation and storage refusal. Update layer-7 architecture
   and deployment instructions with behavior artifacts in the exact reviewed
   implementation head. Keep the kernel, host, pure game fold and identity
   authority unchanged.
3. On the named destination, measure capacity and recovery, prepare exact
   source/image and configuration, and obtain explicit activation authority.

Each implementation request rests on the adopted exact note and the current
behavior it changes. None inherits deployment permission from note delivery.

Before public activation, run the actual TLS/proxy path with the human browser
and supported agent client, retaining the separate two-browser controls.
Prove exact-origin acceptance and wrong Host, cross-origin, null Origin,
forwarded-header spoofing and refusal of direct backend access from the public
network. The owning local account remains inside the existing trust boundary. Repeat byte parity,
anchored seat recovery, exact retry, append-after-push confirmation, writer
exclusion and failure/restart controls. Exercise each resource ceiling and
show no extra append on admission refusal. Cover public GitHub callback
possession/state/PKCE if that provider is enabled. Retain omission controls for
origin enforcement and admission limits, not just successful browser play.

Only then prepare the concrete activation command/configuration and ask Hugh
to approve the named destination. Keep item 6's original promise live until
public delivery is accepted or Hugh explicitly changes its required outcome.
