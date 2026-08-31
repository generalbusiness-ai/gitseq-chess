# gitseq chess

This repository is a complete chess application profile for the gitseq host.
Gitseq verifies each actor's signature and the accepted log order. This module
defines the chess vocabulary, folds that verified log into games, and decides
which recorded acts are effective.

The application is deliberately separate from gitseq. It imports only the
public `host`, `host/identity`, and `host/live` packages, and pins the exact
gitseq commit it was built against in `go.mod`. The fold is pure: it uses record
order and signed sequencer timestamps, with no network, local clock, or mutable
cache.

## Play your first game

Build the binary and initialize a repository for the game records:

```sh
go build -o chess ./cmd/chess
./chess init --repo game-data
```

`init` creates the repository, its sequencer key, and the first player's key.
Its JSON output names that key in `player_key`. There is no separate
key-generation command: when a signed command uses a missing `--key` path,
chess creates that player key securely. This is how `bob.key` is created in the
walkthrough below. Keep every player's key private and use a different key for
each player or agent.

Create a named game as White. The output's `game` value is the durable game
identifier; copy it in place of `<game-id>` in the remaining commands.

```sh
./chess create --repo game-data --name "First game" --color white
```

Join as the second player. Because `bob.key` does not exist yet, this command
creates it before signing Bob's join:

```sh
./chess join --repo game-data --key bob.key --game '<game-id>'
```

Play in UCI notation (`e2e4` means move from e2 to e4), alternating the first
player's managed key and Bob's named key. A promotion appends the chosen piece:
for example, `a7b8q` promotes to a queen; use `r`, `b`, or `n` for another piece.

```sh
./chess move --repo game-data --game '<game-id>' --move e2e4
./chess move --repo game-data --key bob.key --game '<game-id>' --move e7e5
./chess board --repo game-data --game '<game-id>'
```

Each write reports its accepted record identifier and an `effective` decision.
An illegal move or a lost join race remains in the history with a short refusal
reason, but does not change the game.

To watch the board and use temporary presence and chat, start the local web
view and open the printed address:

```sh
./chess serve --repo game-data
```

`serve` listens on `127.0.0.1:8080` by default. You may choose another
loopback address with `--listen`; the command refuses non-loopback addresses so
the local live-room service cannot be exposed accidentally. Board, HTTP, and
MCP responses never include key material.

### Invite with a shared secret

An open game accepts the first valid join. To invite someone privately instead,
create the game with a secret. The log contains only the SHA-256 hash until the
join is submitted; the returned link keeps the secret in its URL fragment.

The secret is read from a file, or from standard input when the file is named
`-`. There is no flag that carries the secret itself, because a command
argument is visible in the process table to every account on the machine for as
long as the command runs.

```sh
printf 'shared-once' > invitation.secret
chmod 600 invitation.secret
./chess create --repo game-data --name "Private game" --color white --join-secret-file invitation.secret
```

Naming a source that turns out to be empty is an error rather than an absent
secret, so a mistyped path cannot quietly create an invitation anyone can
accept. Omit the flag when an open seat is what you want.

Give the secret to the opponent out of band, then join with their key:

```sh
./chess join --repo game-data --key bob.key --game '<game-id>' --secret-file invitation.secret
```

### Where the player key lives

By default the key is `chess/player.key` beneath the repository's Git common
directory, and it never leaves it. The directory is opened once and every name
is resolved against that open directory, so a path component replaced while the
command runs cannot redirect where the key is read or written. A symbolic link
standing in for the key file or for the `chess` directory is refused outright,
even one pointing back inside the same directory.

A new key is published by writing it beside its destination and linking it into
place, so the real name never holds a half-written key, and a second process
racing to create the first key cannot overwrite the winner. The file is set to
mode `0600` explicitly rather than left to the process umask. Existing keys
are accepted only at owner-read-only `0400` or owner-read-write `0600`.
The managed `chess` directory must not be writable by group or others. The
temporary hard-link name is removed after publication; a cleanup failure is
reported, including when another process won the publication race.

This managed custody protects the private key from other local accounts under
ordinary POSIX permissions. It does not protect against another process running
as the same account or against an administrator. The custody checks are defined
for POSIX filesystems. On an unsupported platform where the descriptor and mode
guarantees are unavailable, the command fails closed instead of weakening
them. This fail-closed rule covers both managed storage and explicit `--key`
paths.

`--key` names a different file, and that is an escape hatch with a boundary
worth stating plainly. Choosing the location, and everything above the file's
own parent directory, is yours: the program pins that parent and does not
inspect the path that led to it, so ancestor symbolic links are your
responsibility. The parent must already exist. What the program still refuses,
wherever you put the file: a final component that is a symbolic link or is not
a regular file, a file whose mode is not `0400` or `0600`, a file that
changes identity between being named and being opened, and any publication that
is not exclusive. The program pins the operator-chosen parent after opening it,
but does not validate that parent's permissions.

### Rebind an older chess fold

This build can rebind a repository from `chess-fold@0` or `chess-fold@1` to
its current `chess-fold@2` rules:

```sh
./chess rebind --repo game-data
```

The command reads the existing managed `chess/player.key`; it never generates
a key during recovery. Use `--key` only when the initializing key is already in
the explicit custody location you name. No other player key is authorized,
because the actor who signed the repository's first record is the binding
authority.

Before appending the replacement, the command warns that old records were
folded under different rules and may be interpreted differently. Type
`rebind` exactly to confirm. Success prints the old and new fold versions. An
already-current binding, an unknown fold, a missing or unreadable key, or a key
that does not belong to the initializing actor is refused without changing the
record log. Rebinding appends one host binding record; it does not rewrite the
existing Git objects or chess records.

## Web view, identity, read service, and MCP

`chess serve` prints the address of an embedded web interface. Its lobby and
game pages render only the current durable fold. Selecting a square asks the
fold's rules engine for legal destinations; the browser does not carry a
second chess implementation. The page reloads when the verified frontier
moves. A separate process-local live room shows watchers, signed chat, and
legal motion previews. Those previews never move the durable board. Durable
chess moves still use the command line or MCP adapter; the browser's only
durable write surface is the shared host identity vocabulary described below.

Watching is keyless. Creating a browser identity or joining presence creates
one non-exportable Ed25519 private key in browser memory, proves possession to
the server, and reuses the same key for identity endorsements. The private key
and the server-minted live lease credential remain in memory only. Reloading or
closing the tab loses both and a fresh session has a different public
fingerprint. It stores neither the key nor its previous public fingerprint, so
each session stands visibly on its current fingerprint alone. The server derives
white, black, or watcher status from the folded seats through
`Projection.SeatFor`; the browser cannot claim a role.
That query is a live preview at the last-record instant. It is position-exact
and timestamp-optimistic: it may say yes where a later append refuses on
expiry, never the reverse. Durable move acceptance remains the judgment. Live
state has its own cursor, resets when the process restarts, and is never
presented as durable history.
Drag and submit hints are bounded presence values, not chat history, and vanish
when the lease renews or expires. A newly generated browser key is normally a
watcher for CLI-created games; the page can receive and animate hints from an
authorized seated session.

The identity panel reports persistent identity, vouching, and verification as
separate facts from the current session fingerprint. “Unanchored” never means
recoverable: if an exact-key seat is lost before an effective chess act binds
it to an anchor, a different key cannot recover that seat. A new tab key may be
anchored to the same persistent identity and chess scope through either:

- same-origin GitHub OAuth with state and PKCE, followed by a deployment-witnessed
  host anchor; or
- a NIP-07 signer, which receives the exact server-produced event object and
  returns a Nostr root signature carried in the durable anchor.

An already anchored tab key can also endorse an agent-key fingerprint. Every
endorsement has an explicit `chess` or `chess:<game>` scope and an expiry. The
browser signs the host's exact prepared bytes with its tab key; it does not
encode an alternative identity payload or signing intent. OAuth tokens and
PKCE verifiers stay transient in the server. Provider tokens, root private
keys, tab private keys, live credentials, and bearer values are never placed
in browser storage, URLs, server storage, logs, or error text. The OAuth
authorization URL contains only protocol values such as the one-shot state and
PKCE challenge, not any of those secrets.

GitHub anchoring is disabled unless the repository already declares a
`github` witness and chess serve can reach the deployment's signer through the
absolute Unix-socket path in `GITSEQ_CHESS_IDENTITY_WITNESS_SOCKET`. Configure
`GITSEQ_CHESS_GITHUB_CLIENT_ID`, `GITSEQ_CHESS_GITHUB_CLIENT_SECRET`, and
`GITSEQ_CHESS_GITHUB_REDIRECT_URL`; the redirect must be the exact loopback
`/v1/identity/github/callback` URL served by this process. The authorization,
token, and user endpoints default to GitHub. Deployments may replace them with
fixed values through `GITSEQ_CHESS_GITHUB_AUTHORIZE_URL`,
`GITSEQ_CHESS_GITHUB_TOKEN_URL`, and `GITSEQ_CHESS_GITHUB_USER_URL`, for example
to use a loopback test provider. Partial or mismatched configuration fails
closed when `chess serve` starts.

Chess serve never opens or receives the witness private key. After the GitHub
lookup, it prepares the exact host identity endorsement with a stable retry
key and sends only `host.ActorSigningBytes` to the socket. The signer protocol
uses one connection per signature. Each frame begins with a four-byte
big-endian length. The request body is the signing bytes. The response body is
exactly 96 bytes: the 32-byte Ed25519 public key followed by the 64-byte
signature. The signer must hold the private key outside the chess serve
process. Chess bounds the request and response, applies a three-second
deadline, requires the returned key to equal the currently declared GitHub
witness, verifies the signature, and gives the unchanged prepared act to
`Workspace.AppendSigned`. Refusal, timeout, malformed response, key rotation,
signature failure, or append failure produces the same generic error and no
effective identity anchor.

The same server exposes bounded, read-only JSON endpoints:

- `GET /v1/games?limit=100&after=<game>`
- `GET /v1/board?game=<game>`
- `GET /v1/legal?game=<game>&from=e2`

The service never receives a browser private key and has no HTTP endpoint for
durable chess acts. Use the command line or MCP adapter for signed moves, joins,
draws, and resignations. Identity status is read from the folded
`host/identity` projection; a recorded but ineffective, expired, or revoked
endorsement is not presented as an active anchor.

`chess mcp --repo game-data --key agent.key` runs a newline-delimited JSON-RPC
MCP adapter on standard input and output. It offers bounded game listing, board
and legal-destination queries, plus create, join, move, resign, draw-offer,
draw-accept, and shared host identity lifecycle tools. `anchor` reports whether
the durable endorsement created recovery authority, was refused by the host
identity fold, or could not be read after append. `list_anchors` lists a bounded
set of standing anchors filtered by exact subject, scope, or both.
`revoke_anchor` withdraws an anchor or delegated credential by its record
identifier and reports whether the withdrawal took force. The adapter's process
owns the chosen key; tool results contain only public record data and fold
decisions.

## Durable rules

- A create chooses the creator's color and an optional opponent key or hashed
  join secret. No invitation means an intentionally open, first-valid-join seat.
  An unnamed game uses `chess/create@0`. A named game uses the single atomic
  `chess/create-named@0` act, so a failed name cannot leave behind an open game
  whose identifier was never returned to the caller.
- The current application binding is `chess-fold@2`. Fold @1 introduced the
  creator-only, display-only `chess/name@0` act without changing the exact bytes
  or judgments of `chess/create@0`; fold @2 still understands those records but
  adds the combined named-create judgment. A repository's binding is exact, so
  this build cannot open existing repositories bound to fold @0 or fold @1, and
  those older builds cannot open a repository bound to fold @2.
- A join rests on the create. The first qualified join in log order seats.
- The first move rests on the accepted join; every later move rests on the
  preceding accepted move. Wrong-turn, stale-chain, and illegal moves are
  recorded but ineffective.
- Checkmate, stalemate, repetition, the fifty-move rule, and insufficient
  material are computed by the fold's single rules engine. There is no result
  act that a player can use to claim a win.
- Resignations and draw offers name the current move chain. A draw acceptance
  rests on the exact pending offer and only the other seated player may accept.
- `gitseq/identity-anchor@0` is shared host vocabulary; chess does not create
  its own identity system. A seat belongs to a chess-scoped anchored identity
  when one is in force at the exact create, join, or later act position. The
  deliberate seat threshold accepts every anchor strength the host currently
  resolves: witnessed or self-signed vouching, with live-lookup or in-log
  verification. Unknown or newly introduced strength values confer no seat
  authority until this policy is reviewed. An unanchored or wrongly scoped
  key belongs only to its exact session key. A seated key may therefore
  play first and upgrade later, and another currently anchored key for that
  identity can recover the seat. Only an effective chess act commits that
  late upgrade, and one persistent identity cannot occupy both colors in a
  game. Anchor, delegation, revocation, and move records that share one signed
  second still take force only in verified log order. Expiry continues to use
  the record's signed timestamp.
- Identity mutation results come from folding the verified host state after the
  append, not from assuming that a durable write conferred authority. An
  endorser without standing still writes an attributable anchor, but `anchor`
  reports it as refused. A revocation signed by anyone other than the original
  endorser is likewise durable and refused, and the anchor remains listed.

Run `go test -count=1 ./...` for fold, mutation-witness, invitation, pagination,
and real external-host integration coverage.
