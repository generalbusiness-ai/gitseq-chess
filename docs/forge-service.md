# Run a forge-confirmed Chess service

This recipe supports one Linux host, one repository on a local POSIX
filesystem, and browsers on that same host. The service listens only on
loopback. Public hosting, independent writer clones and multiple-host failover
still require the separate deployment decision in the adopted
[browser-delivery design](../notes/2026-09-04-browser-delivery.md).

## Prepare the repository and custody

Choose an existing Chess sequence with the binding required by this build.
Record its full genesis and its exact `refs/seq/<genesis>` ref. Provision its
sequencer key separately: it must match that sequence, and a new key cannot
replace it. Startup fetches the named forge ref and verifies the signed
sequence and binding. It never initializes a sequence to recover a missing or
incompatible one.

Use one named Git remote with one identical fetch and push destination.
Configure transport authentication in the repository's local Git config,
a credential helper or SSH configuration. Mount the forge credential privately
at runtime. Keep normal SSH host-key or TLS certificate verification enabled.
The sequencer key and the forge credential are two different secrets.

The example below assumes `game-data` is the provisioned checkout,
`custody/sequencer` is the existing private sequencer key, and `forge-auth`
contains an operator-provided executable credential helper and its credential.
The helper reads its credential from `/forge-auth`; it must never print it in
diagnostics. The checkout's owner runs the container. Paths must be absolute
when mounted; use a filesystem that supports POSIX ownership and `flock`.

```sh
CHESS_GENESIS='<full genesis>'
CHESS_REMOTE='origin'
git -C game-data config --local chess.forgeRemote "$CHESS_REMOTE"
git -C game-data config --local chess.forgeGenesis "$CHESS_GENESIS"
git -C game-data config --local chess.forgeRef "refs/seq/$CHESS_GENESIS"
git -C game-data config --local chess.sequencerKey /custody/sequencer
git -C game-data config --local credential.helper /forge-auth/helper
```

All four `chess.*` values are required together. An incomplete configuration,
multiple values or different push destination refuses instead of falling back
to local writes. They live in the Git common directory, so linked worktrees
share the policy. Chess refuses inherited `GIT_*` settings before entering the
host or using custody, including repository, object and configuration overrides.
Unset them before launching Chess. This also applies to native commands and
read-only CLI/MCP operations. It does not change the process environment around
concurrent requests. Use on-disk Git/SSH configuration and, where
needed, an explicitly mounted SSH agent for transport.

## Build and run

Build an exact reviewed source commit. The Dockerfile pins the Go 1.26.7
Debian toolchain image by digest, verifies the pinned Go modules and keeps the
same image as its runtime so Git and its transport libraries are pinned too.
This deliberately retains the toolchain rather than assembling another
runtime package set. Record the resulting image digest when deploying.
Build from tracked source; keys and game data belong outside the build context.

```sh
CHESS_SOURCE='<full reviewed source commit>'
CHESS_CONTEXT=$(mktemp -d)
git archive "$CHESS_SOURCE" | tar -x -C "$CHESS_CONTEXT"
docker build --build-arg CHESS_SOURCE="$CHESS_SOURCE" \
  -t "gitseq-chess:$CHESS_SOURCE" "$CHESS_CONTEXT"

docker run --rm --name chess --network host \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$PWD/game-data,dst=/data" \
  --mount "type=bind,src=$PWD/custody,dst=/custody,readonly" \
  --mount "type=bind,src=$PWD/forge-auth,dst=/forge-auth,readonly" \
  "gitseq-chess:$CHESS_SOURCE" \
  serve --repo /data --listen 127.0.0.1:8080
```

Open `http://127.0.0.1:8080` in browsers on that Linux host. There is no port
publication or proxy that rewrites Host or Origin. Docker Desktop on another
operating system is a different network boundary and is not covered here.
For a rootless container runtime, use its mapping of the owning host account;
passing the host UID inside that mapping may select a different account.
The acceptance fixture uses rootless containerd on Linux, with container UID 0
mapped to the owning host UID, and the same host-network loopback boundary.

The service needs no player or Nostr root private key. Tab keys remain
nonexportable and live only in their browser tab. Nostr signatures come from
the browser's external signer. GitHub login additionally requires the existing
provider configuration and an external witness signer whose public key is
durably declared. Configure those through the existing identity service
options; never mount the witness private key into Chess.

## Ownership and recovery

Every supported CLI, MCP and HTTP writer acquires
`chess-writer.lock` in the real Git common directory. It is a private,
owner-only regular file held with an exclusive OS lock. CLI holds it until
exit; MCP holds it from its first write until that process exits; `serve`
holds it for its lifetime. Another writer refuses, including one started
through a linked worktree. Read-only CLI/MCP queries remain available.
Do not unlink a held lock. Process death releases the OS lock; replacement
of the file or loss of its descriptor stops durable intake.

In forge mode, a local append is pending until the configured remote confirms
its exact head. The service serializes append and confirmation, refuses new
durable intake while pending, and serves board, identity, OAuth completion
and live-role decisions from one verified confirmed prefix. A confirmed act
may still be refused by the chess fold. Local mode keeps its local-durability
behavior, with the same writer ownership rule.

Confirmation makes at most three attempts, with 100 ms then 200 ms backoff,
a five-second limit per Git operation and a twelve-second overall limit.
It checks the remote after a push even when the response was lost. A verified
exact match publishes success; another attempt never invents a new signature
or sequence. Transport errors leave the last confirmed view available and
preserve the pending local history. A retry of a live browser draft can
reconcile that original append; after restart or draft expiry, inspect the
confirmed board before choosing another action. Startup reconciles retained
pending history. If a startup attempt remains unavailable, restore transport
and restart the service to retry reconciliation before accepting new work.

`refs/chess/forge-observed/<genesis>` is a fetch observation and may move.
`refs/chess/forge-confirmed/<genesis>` retains the verified published prefix.
A forge rollback behind that retained prefix or a divergent history refuses;
retain the checkout and forge objects for operator reconciliation. Do not
force-push, rebase signed records, delete pending history or run `init` as a
repair. Rebinding in forge mode also requires an explicit operator procedure.
These guards coordinate one owning account and cooperating Chess processes;
they cannot fence another clone or an administrator modifying Git directly.

## Verification

The repository tests exercise shared ownership, linked worktrees, process
death between append and push, restart and rollback refusal, failed pushes,
lost responses, last-attempt confirmation and temporary ancestry-check
failures. Pending board, identity and live-seat reads remain on the confirmed
prefix. Separate omission mutants must fail when early acknowledgment,
pending-head reads, writer locking or the Git-environment boundary is removed.

Linux browser acceptance uses two isolated Chromium contexts, the actual
container service and an authenticated loopback Git HTTP fixture. It plays to
checkmate, recovers a Nostr-bound seat with fresh tab keys, and retries an
aborted response without another record. Wrong and unanchored keys refuse.
Expired and revoked key fixtures exercise the real browser signing and HTTP
submission path; their preprovisioned keys are test inputs, while the normal
players use actual memory-only generated tab keys. The workroom artifact
records the exact source, image, commands, record counts and results.
