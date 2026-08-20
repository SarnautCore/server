# SarnautCore server

This repository contains the Go services for SarnautCore. The shard currently hosts a fixed-rate zone, loads NPC placements from the private runtime data repository, and replicates authoritative movement over QUIC.

## Architecture

```mermaid
flowchart LR
    Client[Game client] --> Gateway[gateway]
    Gateway -->|QUIC + protobuf| Shard[shard modular monolith]
    Auth[auth] <-->|NATS| Gateway
    Auth <-->|NATS| Shard
    Shard --> Modules[world / combat / quests / chat]
    Auth --> PostgreSQL[(PostgreSQL)]
    Shard --> PostgreSQL
    Auth --> Valkey[(Valkey)]
    Shard --> Valkey
    Gateway --> Valkey
    Gateway -. traces .-> OTel[OTel collector]
    Auth -. traces .-> OTel
    Shard -. traces .-> OTel
```

`shard` is a modular monolith. Its game systems stay under `internal` and communicate through Go interfaces. `internal/world` owns the simulation and exposes domain types only; all protobuf mapping lives in `internal/session/mapping.go`, so a second wire format is a second mapping file and not surgery on the sim ([ADR 0028](https://github.com/SarnautCore/docs/blob/main/adr/0028-world-sim-protobuf-boundary.md)). `internal/combat` registers as a per-tick `world.System` and resolves every gameplay rule — ability range, damage and cooldown, mob level and health, faction hostility, aggro and leash radii, respawn window — from the loaded content pack, so a second ability or a second mob is a content change and nothing else. `auth` is a separate service holding the only credentials for the `auth` schema, and `gateway` is the thin client edge — which, for M2, carries no player traffic: the client connects to the shard directly and the shard redeems the ADR 0030 ticket itself ([ADR 0033](https://github.com/SarnautCore/docs/blob/main/adr/0033-m2-module-topology-gateway-deviation.md) §3). NATS connects processes. PostgreSQL and Valkey are **required** by both `auth` and `shard`: accounts and character state have nowhere else to live, and sessions, tickets and play locks have no fallback. QUIC sits behind `internal/transport`, so a later raw UDP implementation does not change the session protocol.

### Admission

A connection is refused unless its `EnterZoneRequest` carries a live shard ticket. Tickets are minted out of band by the auth service and burned on redemption, so authentication happens before the client opens a game connection:

```
POST /v1/accounts   register
POST /v1/sessions   log in, receive an account session token (sarnaut_as_..., 12 h)
GET  /v1/chargen/options   the character-creation options the pack carries
POST /v1/characters create a character
POST /v1/tickets    mint a single-use shard ticket (sarnaut_tk_..., 60 s)
```

Passwords are Argon2id in a PHC string; tokens are opaque random bytes whose SHA-256 digest is the only form ever stored. No password, email address or token value appears in any log line ([ADR 0030](https://github.com/SarnautCore/docs/blob/main/adr/0030-auth-account-service-session-tokens.md)).

Wire definitions live in `proto/sarnaut/v1`. Generated Go code is committed under `gen/sarnaut/v1`.

Every post-handshake frame in either direction is a `ClientMessage` or a `ServerMessage` from `proto/sarnaut/v1/envelope.proto`, on either carrier: a datagram carries a whole envelope, not a bare submessage. Only `ClientMessage.move_intent` and `ServerMessage.snapshot_batch` are eligible for datagrams; everything else, combat included, uses the reliable ordered stream. An unset or unrecognised oneof case is refused with `ServerMessage{error: UNSUPPORTED_MESSAGE}` and a close (ADR 0026).

`proto/PROTO_LOCK.sha256` pins the digests of that `.proto` set and is byte-identical to the copy the client repository commits. `./scripts/generate.ps1` and `make generate` refresh it alongside `gen/`; CI runs `./scripts/proto-lock.ps1 -Check` and fails on a proto edited without regenerating (ADR 0027).

## Development quickstart

Requirements:

- Go 1.26
- protoc 35 or later
- `protoc-gen-go` 1.36.12
- golangci-lint 2.12.2 for local linting

Generate, test, lint, and build on Windows:

```powershell
./scripts/generate.ps1
./scripts/test.ps1
./scripts/lint.ps1
./scripts/build.ps1
```

The equivalent Make targets are `make generate`, `make test`, `make lint`, and `make build`.

### Content packs

The shard loads a **compiled runtime pack** and never parses game-design YAML
([ADR 0006](https://github.com/SarnautCore/docs/blob/main/adr/0006-yaml-source-compiled-runtime-data.md),
[ADR 0029](https://github.com/SarnautCore/docs/blob/main/adr/0029-runtime-pack-format.md)).
A pack is a directory holding `manifest.json` and `tables/`, produced by
`sarnaut-pack build` in the `tools` repository.

`SARNAUT_CONTENT_PACK` is required and has no default. There is no fallback
path, so a misconfigured shard fails at startup instead of quietly serving
something else. Packs compiled from the private `data` repository are private
artifacts and are never committed here.

The one pack in this repository is the golden fixture at `testdata/packs/demo`,
compiled from the hand-authored `data-schemas/demo` dataset. Server tests load
it, and it is enough to run a shard locally:

```powershell
$env:SARNAUT_CONTENT_PACK = "$PWD\testdata\packs\demo"
```

Both `auth` and `shard` need PostgreSQL, Valkey and NATS, and both refuse to
start without them. Bring the development infrastructure up first, then apply
the schema:

```powershell
docker compose -f ..\infra\compose\docker-compose.yml up -d
$env:SARNAUT_POSTGRES_DSN = "postgres://sarnaut:sarnaut_dev@127.0.0.1:5433/sarnaut?sslmode=disable"
$env:SARNAUT_VALKEY_ADDRESS = "127.0.0.1:6379"
$env:SARNAUT_NATS_URL = "nats://127.0.0.1:4222"
go run ./cmd/migrate up
```

Then run the services in separate PowerShell terminals:

```powershell
go run ./cmd/auth
go run ./cmd/shard
go run ./cmd/gateway
```

`auth` reads its character-creation options from the same pack the shard loads,
so `SARNAUT_CONTENT_PACK` is required by both.

The shard states that pack's digest in its `ServerHello` and refuses a client
that names a different one, or none at all
([ADR 0027](https://github.com/SarnautCore/docs/blob/main/adr/0027-proto-contract-and-wire-evolution.md),
`protocol/session.md` rule 5.1.4). A client that has not shipped a pack of its
own yet — which today includes the Godot client and `cmd/probe` without `-pack` —
needs the shard told to take it:

```powershell
$env:SARNAUT_CONTENT_ALLOW_UNVERIFIED_PACK = "true"
```

The alternative is to hand the client the digest the shard logs at startup as
`pack_id`, which is what the smoke scripts do.

To see what a pack resolves to without starting a listener:

```powershell
go run ./cmd/shard -dump-spawns spawns.json
```

The gateway logs `shard handshake complete` after it receives the shard's `ServerHello`. Each process also exposes probes:

```powershell
Invoke-WebRequest http://127.0.0.1:8080/healthz # gateway
Invoke-WebRequest http://127.0.0.1:8081/readyz  # shard
Invoke-WebRequest http://127.0.0.1:8082/readyz  # auth
```

The shard creates an ephemeral self-signed certificate at startup. The gateway's development TLS configuration accepts it. This is local-only behavior.

Use the probe to enter the zone, send movement input, and print the number of
snapshot packets and entity records received. Because the shard admits nobody
without a ticket, the probe can do the whole out-of-band flow itself — register,
log in, create the named character, mint a ticket:

```powershell
go run ./cmd/probe -duration 5s -auth http://127.0.0.1:8083 -character Probeling
```

Pass `-ticket` instead to present one you already have, and `-expect-refusal` to
assert that a connection with no ticket is turned away.

### The M2 admission slice

```powershell
./scripts/m2-auth-slice.ps1
```

It boots `auth` and a shard against the vendored fixture pack and walks the whole
path: an unauthenticated connection is refused, then register, log in, create a
character, enter the zone at the chargen option's spawn, and reconnect to the
position the disconnect checkpoint saved. It asserts that neither log carries a
password, an email address or a ticket.

### M2 vertical slice driver

`scripts/m2-slice-driver` plays the slice headlessly and prints one line per
step:

```powershell
go run ./scripts/m2-slice-driver
```

```
PASS host     in-process shard on 127.0.0.1:53312, zone M2Slice, pack 93d786dc...
PASS connect  zone=M2Slice entity=6 datagrams=true server_pack=""
PASS target   mob.paper-harbor.tide-crab entity=3 level=2 health=120/120
PASS cast     6 casts of ability.melee.harbor-cleave for 20 damage each
PASS kill     mob.paper-harbor.tide-crab died on cast 6 to 120 total damage
PASS logout   clean exit requested
```

With no `-address` it stands a shard up in process from the vendored fixture
pack, so it needs nothing running and no configuration; pass `-address` to
drive one that is already up. It exits non-zero if any step fails, and CI runs
it on every push. Every later M2 server task extends this driver rather than
writing one of its own.

### Godot client integration smoke

With the `server` and `client` repositories in the same parent directory, run the cross-repository SAR-20 smoke:

```powershell
./scripts/sar20-client-smoke.ps1
```

The script builds and starts the real shard **and the real auth service** against the vendored fixture pack, performs the out-of-band login flow, hands the resulting ticket to the client's reusable .NET transport harness, and fails unless the joined player's position advances in an authoritative snapshot. The .NET client has no login UI of its own yet; that lands in the next wave. It uses ports `4342`, `8181`, `8182` and `8183` by default so it does not disturb services on the development ports. Override `-ClientRepository`, `-Address`, `-HealthAddress`, `-AuthAddress` or the infrastructure endpoints when needed.

The Godot client uses `System.Net.Quic` over MsQuic. The public .NET 10 API has no QUIC datagram send or receive methods, so the connection does not negotiate datagrams and this server uses its ordered QUIC-stream fallback. Frames on that stream use the same 4-byte big-endian protobuf length prefix as `internal/transport`.

Copy `config.example.yaml`, set `SARNAUT_CONFIG` to its path, and override individual values with `SARNAUT_*` variables when needed. The main connection variables are:

| Variable | Purpose |
| --- | --- |
| `SARNAUT_SHARD_ADDRESS` | Gateway target, default `127.0.0.1:4242` |
| `SARNAUT_QUIC_LISTEN_ADDRESS` | Shard QUIC listener |
| `SARNAUT_HEALTH_ADDRESS` | Per-process health listener |
| `SARNAUT_CONTENT_PACK` | Compiled runtime pack directory. Required by the shard; no default |
| `SARNAUT_CONTENT_ALLOW_EXTRA` | Accept a pack built with `--keep-extra`, default `false` |
| `SARNAUT_CONTENT_ALLOW_UNVERIFIED_PACK` | Admit a client that names no pack, default `false` |
| `SARNAUT_WORLD_ZONE_ID` | Network zone ID, default `InstLeague1` |
| `SARNAUT_WORLD_TICK_INTERVAL` | Fixed simulation interval, default 30 Hz |
| `SARNAUT_WORLD_SNAPSHOT_INTERVAL` | Replication interval, default 15 Hz |
| `SARNAUT_WORLD_SPAWN_SEED` | Seeds the zone spawn stream that draws mob levels and respawn delays, default `0` |
| `SARNAUT_WORLD_SEED` | Per-shard-instance half of the loot roll seed (mechanics/loot.md rule 5.2.4), default `sarnaut-shard` |
| `SARNAUT_NATS_URL` | NATS server URL |
| `SARNAUT_POSTGRES_DSN` | PostgreSQL connection string |
| `SARNAUT_VALKEY_ADDRESS` | Valkey host and port. Required by `auth` and `shard` |
| `SARNAUT_AUTH_LISTEN_ADDRESS` | Auth HTTP API listener, default `127.0.0.1:8083` |
| `SARNAUT_AUTH_NAME_BLOCKLIST` | Comma-separated blocked name substrings; `-` disables |
| `SARNAUT_AUTH_REQUEST_TIMEOUT` | Shard-to-auth NATS request timeout, default `2s` |
| `SARNAUT_SHARD_INSTANCE_ID` | Play-lock holder id; derived from host and pid when empty |
| `SARNAUT_OTEL_ENDPOINT` | OTLP gRPC collector host and port |

## About SarnautCore

This repository is part of SarnautCore, a fan-driven, non-commercial, open-source recreation kit for Allods Online.

The project charter and architecture decision records live in [SarnautCore/docs](https://github.com/SarnautCore/docs). Read those before opening a pull request here.

## Clean-room posture

SarnautCore is built clean-room. This project never distributes game assets or data owned by MY.GAMES. Anything you run through the kit comes from your own copy of the game, on your own machine.

## License

AGPL-3.0. See [LICENSE](LICENSE).
