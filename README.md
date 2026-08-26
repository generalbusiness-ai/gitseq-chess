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

## Run locally

Build the one binary, then initialize a data repository:

```sh
go build -o chess ./cmd/chess
mkdir game-data
./chess init --repo game-data
./chess serve --repo game-data
```

`serve` listens on `127.0.0.1:8080` by default. You may choose another
loopback address with `--listen`; the command refuses non-loopback addresses so
the local live-room service cannot be exposed accidentally.

`init` creates the repository's binding, sequencer key, and first local player
key. Board, HTTP, and MCP responses never include key material.

Create a game with an open seat:

```sh
./chess create --repo game-data --color white
```

Or create a secret invitation. The log contains only the SHA-256 hash until the
join is submitted; the returned link keeps the secret in its URL fragment.

The secret is read from a file, or from standard input when the file is named
`-`. There is no flag that carries the secret itself, because a command
argument is visible in the process table to every account on the machine for as
long as the command runs.

```sh
printf 'shared-once' > invitation.secret
chmod 600 invitation.secret
./chess create --repo game-data --color white --join-secret-file invitation.secret
```

Naming a source that turns out to be empty is an error rather than an absent
secret, so a mistyped path cannot quietly create an invitation anyone can
accept. Omit the flag when an open seat is what you want.

Use another key file for the opponent, then play in UCI notation:

```sh
./chess join --repo game-data --key bob.key --game '<create-record>' --secret-file invitation.secret
./chess move --repo game-data --game '<create-record>' --move e2e4
./chess move --repo game-data --key bob.key --game '<create-record>' --move e7e5
./chess board --repo game-data --game '<create-record>'
```

Give each player or agent a different key file. A command reports both the
accepted record identifier and the fold's `effective` decision; illegal moves
and lost join races remain in the history with a short refusal reason.

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

## Web view, read service, and MCP

`chess serve` prints the address of an embedded, read-only web interface. Its
lobby and game pages render only the current durable fold. Selecting a square
asks the fold's rules engine for legal destinations; the browser does not carry
a second chess implementation. The page reloads when the verified frontier
moves. A separate process-local live room shows watchers, signed chat, and
legal motion previews. Those previews never move the durable board.

Watching is keyless. Joining presence or chat creates a non-exportable
Ed25519 private key in browser memory, proves possession to the server, and
keeps the server-minted lease credential in memory too. Reloading or closing
the tab loses both. The server derives white, black, or watcher status from the
folded seats through `Projection.SeatFor`; the browser cannot claim a role.
That query is a live preview at the last-record instant. It is position-exact
and timestamp-optimistic: it may say yes where a later append refuses on
expiry, never the reverse. Durable move acceptance remains the judgment. Live
state has its own cursor, resets when the process restarts, and is never
presented as durable history.
Drag and submit hints are bounded presence values, not chat history, and vanish
when the lease renews or expires. A newly generated browser key is normally a
watcher for CLI-created games; the page can receive and animate hints from an
authorized seated session, while durable browser seat custody and submission
remain outside this service.

The same server exposes bounded, read-only JSON endpoints:

- `GET /v1/games?limit=100&after=<game>`
- `GET /v1/board?game=<game>`
- `GET /v1/legal?game=<game>&from=e2`

The durable browser write path belongs to later work. This service accepts only
the public half and signatures for its ephemeral live room; it never receives
a private key and has no HTTP endpoint for durable chess acts. Use the command
line or MCP adapter for signed moves, joins, draws, and resignations.

`chess mcp --repo game-data --key agent.key` runs a newline-delimited JSON-RPC
MCP adapter on standard input and output. It offers bounded game listing, board
and legal-destination queries, plus create, join, move, resign, draw-offer,
draw-accept, and shared host identity anchor acts. The adapter's process owns
the chosen key; tool results contain only public record data and fold decisions.

## Durable rules

- A create chooses the creator's color and an optional opponent key or hashed
  join secret. No invitation means an intentionally open, first-valid-join seat.
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
  when one is in force at the exact create, join, or later act position;
  otherwise it belongs to the exact session key. A seated key may therefore
  play first and upgrade later, and another currently anchored key for that
  identity can recover the seat. Only an effective chess act commits that
  late upgrade, and one persistent identity cannot occupy both colors in a
  game. Anchor, delegation, revocation, and move records that share one signed
  second still take force only in verified log order. Expiry continues to use
  the record's signed timestamp.

Run `go test -count=1 ./...` for fold, mutation-witness, invitation, pagination,
and real external-host integration coverage.
