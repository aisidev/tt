# Implementation plan — TTstack in Go

Status: reviewed against Rust revision `1ad83a9feed5fac820d74b5e7ef752e50a552740`
(workspace version 0.6.0). The subsequently authorized optional recovery
protocols and their forward-only database migrations are specified in
[compatibility §10–15](compatibility.md#10-durable-command-envelope-optional-extension);
they are outside pinned Rust parity. The original phases 0–7 are implemented and accepted within the recorded native matrix. This document is the contract for
what will be built, how it maps onto the Rust release, and how each step is verified.

Current development exception: before v1.0, retain DB versioning infrastructure
without incrementing schema versions for feature iterations or requiring older
development DB compatibility. The
[DB schema policy](development.md#db-schema-policy-before-v10) takes precedence
over this historical plan's state-interchange and migration acceptance rules.

## 1. Purpose

TTstack is a lightweight private cloud for developers and small teams: a CLI
(`tt`), a controller (`tt-ctl`) and a per-host agent (`tt-agent`) that together
create, access, stop, restart and delete VMs and containers on a small fleet.
The Rust workspace at `../TTstack` is the reference implementation. The revision
above freezes this delivery's oracle; fixtures and live reports must record it.
Changes in the reference during delivery require an explicit compatibility review,
not silent regeneration of expected results.

This repository re-implements the same product in Go, with the same observable
behavior, the same API, and the same persistent state format, so that the two
implementations can be mixed in one fleet and so that an existing deployment can be
taken over by the Go binaries without data conversion.

## 2. Goals and non-goals

Goals:

1. Feature parity with the pinned Rust release: all engines, storage backends, networking
   modes, SSH bootstrap, offline resource updates, disk backup, image recipes,
   dashboard, local and distributed deployment.
2. Wire compatibility: identical HTTP routes, JSON documents, status codes, CLI
   output and exit codes (with the documented process-specific error diagnostic
   exception in compatibility §7).
3. State compatibility: identical SQLite files (schema versions and table layouts),
   identical on-disk runtime paths and identifiers, identical service files.
4. Idiomatic Go: `cmd/` binaries, `internal/` domain packages, `context`, `net/http`,
   `database/sql`, table-driven tests. Structure follows Go practice, not the Rust
   module tree.
5. Minimal dependencies: the standard library plus four direct modules (see §6).
   Their required transitive dependencies are pinned in `go.mod`/`go.sum` too.

Non-goals (matching the Rust product boundaries):

- No new features, no "improvements" that change behavior. Anything not in the Rust
  release does not belong here.
- No guest migration, high availability, distributed storage, image distribution,
  automatic guest restart after host reboot, or per-owner authorization.
- No TLS or identity system for the service APIs; authentication stays a single
  shared bearer key.

## 3. Why Go

TTstack is a control plane, not a compute engine. It schedules workloads, keeps
state in SQLite, serves HTTP, drives hypervisors and container runtimes through
their CLIs and devices, and manages files, ZFS snapshots and firewall rules. The
guest workload does not run inside the control-plane process. Most expected
control-plane work is process management and network/database waits; this makes
Go a reasonable fit, but does not establish equal latency or memory use without
measurement. The language choice is driven by engineering properties:

1. **Build and iteration time.** The Rust workspace pulls in tokio, axum,
   reqwest with rustls/AWS-LC and a bundled SQLite, so a clean release build
   needs a C/C++ toolchain. The Go SQLite driver is pure Go, removing that native
   build dependency. Faster iteration is an expected benefit; build-time claims
   require comparable clean and incremental measurements once the port exists.

2. **Cross-compilation for the fleet.** `GOOS=linux|freebsd GOARCH=amd64 go
   build` produces any target from any machine with no cross toolchain. This
   port keeps the FreeBSD bhyve/jail paths, validated on FreeBSD 15.x only, and the Rust release
   needs a separate target toolchain and libc-aware builds for the same reach —
   its deployment guide has to warn that a glibc binary cannot run on a musl
   host.

3. **Self-contained static binaries.** A Go build can be one statically linked
   executable with no libc or shared-library coupling, which is exactly what the
   deployment path needs: copy the file over SSH, verify it, restart the
   service. Release builds explicitly use `CGO_ENABLED=0`; kernel ABI support,
   external engine tools, certificates and host prerequisites still matter.

4. **A complete standard library.** Server and client HTTP (including redirect
   control and Unix-socket transports), JSON, database/sql, process execution,
   hashing, filesystem handling, flags and `httptest` all ship with the
   toolchain. The Rust release needs roughly ten external crates plus native
   dependencies for the same surface; the Go port keeps a closed set of four
   direct modules plus their transitive dependencies (see §6).

5. **A concurrency model that matches the problem.** The system is many
   concurrent, mostly-waiting operations: per-environment mutation locks, 15 s
   reconciliation loops, 60 s expiry sweeps, bounded worker pools, timeouts and
   cancellation. Goroutines, channels and `context` express that directly, with
   no async function coloring and no `spawn_blocking` plumbing for synchronous
   storage work.

6. **Operational tooling.** The race detector, `pprof`, coverage and the uniform
   `gofmt`/`go vet`/`go test` toolchain keep a small codebase verifiable, and a
   dependency-light tree is easier to hand to contributors and to audit.

What is given up, and how it is handled:

- Rust's type system catches classes of defects at compile time (exhaustive enum
  matching, ownership, no nil dereference). Go compensates with a small,
  explicit, test-heavy codebase: strict enum marshaling, validation at every
  boundary, the same conservative lifetime rules as the reference, and golden
  fixtures. Crucially, the Rust implementation is the behavioral oracle, so
  parity is *verified* against a running reference rather than argued from types.
- Garbage collection and runtime overhead must fit the control-plane workload.
  The port has no measured memory or latency baseline yet; bounded functional
  validation checks practical behavior without claiming benchmark equivalence.

## 4. Confirmed decisions

| Decision | Choice |
|---|---|
| Compatibility target | Full wire compatibility, including SQLite schema versions v5 (controller) and v6 (agent) |
| FreeBSD engines | Ported (`bhyve`, `jail`, PF paths), runtime-gated to FreeBSD hosts exactly as in Rust |
| Dependencies | Standard library + `modernc.org/sqlite`, `github.com/google/uuid`, `github.com/BurntSushi/toml`, `golang.org/x/sys` |
| Delivery | Phased (0–7), each phase builds, vets, tests and is committed separately; prerequisite plan corrections are a separate documentation commit |
| Documentation language | English (this repository), same convention as TTstack |
| Author metadata | `fh <hui.fan@mail.ru>`, matching TTstack |
| Module path | `github.com/aisidev/tt` |

## 5. What "compatible" means precisely

Three levels, all in scope:

1. **API level.** Same routes, methods, request/response JSON field names and
   omission rules, stable business error messages, and HTTP status mapping (including the
   error-string-prefix mapping used by the backup protocol). An unmodified Rust
   `tt` CLI must work against a Go `tt-ctl`, and a Rust `tt-ctl` must be able to
   drive a Go `tt-agent`.
2. **State level.** Same SQLite schema version numbers, same tables, indices and
   pragmas, same JSON blob layout inside rows, same `_meta` keys, same
   `service.lock` behavior. Same runtime paths (`/home/ttstack/run`, clone
   directories, image directories, backup directories, `@ttsnap` / `@ttbackup-*`
   snapshot names and ZFS user properties).
3. **Host-visible identifiers.** Names that outlive a process or are shared with
   the host kernel must be produced bit-exactly: TAP names (`tt-` + 12 hex digits
   from a frozen SipHash-1-3 over the VM id plus a `0xff` terminator), guest MAC
   addresses (`02:54:aa:bb:cc:dd`, hexadecimal IPv4 octets), bridge `tt0`/`10.10.0.1/16`,
   nftables tables/chains, cgroup names and jailer UIDs, PID/log/socket file
   names, and systemd unit names.

Details, field by field, are in [compatibility.md](compatibility.md).

## 6. Technology choices

| Concern | Rust release | Go implementation |
|---|---|---|
| HTTP server | axum | `net/http` with an explicit raw-path router (no automatic path cleanup) |
| HTTP client | reqwest (rustls) | `net/http` with redirects disabled and per-request timeouts via `context` |
| Async runtime | Tokio | goroutines, `context.Context`, channels; a small counting semaphore type |
| Database | rusqlite (bundled SQLite) | `database/sql` + `modernc.org/sqlite` (pure Go, no cgo) |
| CLI parsing | clap | an explicit subcommand tree with standard-library parsing |
| JSON | serde | `encoding/json` with explicit presence/default/null validation and per-type codecs; see compatibility §2 |
| TOML | toml crate | `github.com/BurntSushi/toml` |
| UUID | uuid crate | `github.com/google/uuid` |
| SHA-256 | sha2 crate | `crypto/sha256` |
| File locking, ioctls, statfs | nix | `golang.org/x/sys/unix` |
| Dashboard assets | embedded HTML string | `//go:embed` of `internal/ctl/web/index.html` |
| Errors | `ruc::Result` with context macros | `error` values wrapped with `fmt.Errorf("…: %w", err)`; HTTP layer flattens to strings |

Deliberate behavioral equivalence, exercised by tests: bounded command execution
with a 300 s default timeout and 4 MiB per-stream capture; 15 s background loops;
8-way concurrent host refresh; 5 s/360 s/600 s client timeouts; WAL with
`synchronous=FULL`.

### Deliberate deviations (documented, not behavioral regressions)

- Downloads in the image builder and release/deploy helpers use Go's HTTP client
  instead of shelling out to `curl`. Same URLs, same SHA-256/SHA-512 verification,
  same atomic publication, same error text where observable.
- Firecracker control calls go through a `net/http` client bound to the VM's Unix
  socket instead of `curl --unix-socket`. Same endpoints, same bodies, same 5 s
  timeout, same failure text contract at the API boundary.
- Guest log capture and process supervision use Go primitives (goroutine reaper,
  `os/exec`) rather than detached threads, with identical file names and contents.

## 7. Repository layout

```text
tt/
├── cmd/
│   ├── tt/            # CLI binary
│   ├── tt-ctl/        # controller binary
│   ├── tt-agent/      # host agent binary
│   └── upgrade-agent-v5/  # offline v5→v6 database converter
├── internal/
│   ├── model/         # entities, enums, constants, validation
│   ├── api/           # wire DTOs and the ApiResp envelope
│   ├── capability/    # design matrix, scoped host report, legacy tag gates
│   ├── backup/        # backup protocol types and replay/precondition logic
│   ├── guestconfig/   # opaque guest files, digest, ext4 config disk
│   ├── sshboot/       # SSH options/info, seed script, readiness probes
│   ├── auth/          # API-key parsing, constant-time compare, middleware
│   ├── runcmd/        # bounded command execution and small process helpers
│   ├── engine/        # Engine interface, factory, PID recovery; per-engine files
│   ├── netcfg/        # bridge, TAPs, nftables NAT/DNAT/isolation, FreeBSD PF
│   ├── storage/       # ImageStore: file, zvol; backup artifacts and reflink
│   ├── agent/         # agent runtime, HTTP handlers, backup worker, SSH ingress
│   ├── ctl/           # controller DB, scheduler, handlers, dashboard, forwarding
│   └── cli/           # client, command tree, deploy, image builder, ttconfig
├── docs/              # this documentation
├── tools/             # deploy.toml.example and helper scripts
└── .github/workflows/ # CI
```

Wire structures embedded by entities live in the leaf `model` package to avoid
Go import cycles. `backup`, `capability`, `guestconfig` and `sshboot` own behavior;
`api` owns transport wrappers, including the backup view. The concrete dependency
direction is specified in [architecture.md](architecture.md#2-package-layout).

Package-by-package responsibilities and the Rust mapping are in
[architecture.md](architecture.md) and [mapping.md](mapping.md).

## 8. Delivery phases

Each phase ends with `gofmt -l`, `go vet ./...`, `go test ./...`, `go build ./...`
all clean, and its own commit. Details and acceptance criteria per phase are in
[roadmap.md](roadmap.md).

| Phase | Content | Key verification |
|---|---|---|
| 0 | Repository skeleton: `go.mod`, `Makefile`, CI, `cmd/` stubs | builds, vet, empty test run |
| 1 | `model`, `api`, `capability`, `backup`, `guestconfig`, `sshboot`, `auth`, `runcmd` | unit tests plus JSON golden fixtures produced from the Rust build |
| 2 | `storage` (file, zvol, backup artifacts), `netcfg` (Linux + FreeBSD) | unit tests; TAP-name and IP/MAC vectors; `mkfs.ext4`/`debugfs` tests |
| 3 | `engine` (qemu, firecracker + jailer sandbox, docker/podman, bhyve, jail) | unit tests with fake tools; command-line golden tests |
| 4 | `agent`: runtime, handlers, backup worker, SSH ingress, agent DB v6 | `httptest` API tests; schema rejection tests; reconciliation tests |
| 5 | `ctl`: DB v5, scheduler, handlers, dashboard, backup forwarding | mock-agent HTTP workflow tests (interrupted creation, partial deletion, expiry retry, duplicate names, resize conflicts) |
| 6 | `cli`: client, commands, `~/.ttconfig`, local + distributed deploy, image recipes | CLI golden output tests; deploy script tests; recipe dry runs |
| 7 | Compatibility acceptance: cross-implementation tests, converter tool, docs | Go controller ↔ Rust agent and Rust controller ↔ Go agent smoke runs on an authorized host |

## 9. Verification strategy

1. **Golden JSON fixtures.** A checked-in harness built against the pinned Rust
   source emits each wire type and accepted/rejected decode cases, including
   missing/null/default/empty values. Compare JSON semantically, preserving
   integer precision and distinguishing missing, null, empty arrays and objects.
   Compare bytes only where bytes are contractual: CLI text, generated scripts,
   identifiers and guest-config digest input. Fixtures carry oracle provenance
   and are regenerated by a documented command, never during normal Go tests.
2. **SQLite interoperability.** Commit synthetic SQL/JSON fixture exports and
   their Rust generator, not database binaries. Go-only tests reconstruct temporary
   databases. Separate differential checks create real Rust databases, let Go read
   and mutate them, and reopen them in Rust; check both schema gates and migrations.
3. **Behavioral unit tests.** Port the Rust unit tests that encode contracts
   (scheduler placement, capability admission, backup replay, validation,
   SipHash vectors, ext4 growth).
4. **Mock-agent workflow tests.** `httptest` servers stand in for agents to
   exercise creation timeouts, partial deletion, expiry, detach and resize
   recovery end to end, mirroring the Rust controller's test coverage.
5. **Live checks (authorized hosts only).** Phase 7 checks QEMU and Docker in both
   controller/agent pairings, plus Firecracker/config-disk/jailer and eligible
   backup backends. Each operation is tested only on engines that support it;
   Docker is not a resize/backup substitute. FreeBSD compiles in CI. Native
   validation is FreeBSD 15.x only; unavailable prerequisites are reported explicitly.
   Fixed bridge, firewall and run paths require host/namespace isolation as well
   as distinct data directories. Tests are bounded, cleaned up and never disturb
   pre-existing workloads. See the roadmap for the acceptance matrix.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Silent JSON drift (presence/default/null rules differ from serde) | Rust-generated positive and negative decode fixtures, semantic round trips and byte checks for hashed content |
| SipHash-1-3 tap names differ | Port the algorithm with the Rust test vectors as unit tests |
| SQLite driver differences (`modernc.org/sqlite`) | Use identical pragmas; verify with Rust-created fixture databases; single-writer discipline |
| Rust `error`-string → HTTP status coupling | Centralize mapping in one table mirrored from `core/src/backup.rs`; tests enumerate every prefix |
| Scope creep into "improvements" | Non-goals in §2; compatibility document is the review checklist |
| FreeBSD paths untestable on Linux hosts | Same runtime gating as Rust; code review against source; native validation is FreeBSD 15.x only |

## 11. Definition of done

The port is complete when:

1. `make build`, `make test`, `make lint` are clean; Linux race tests and Linux/
   FreeBSD cross-builds pass. CI is green when available; lack of remote CI access
   is reported, never represented as a green run.
2. Golden fixture tests cover every wire type and both database schemas, with
   differential tests against the pinned Rust build in both directions.
3. The CLI reference commands in `README.md` work end to end on a Linux systemd host.
4. A Rust controller can drive a Go agent and a Go CLI can drive a Rust controller
   on the same schema versions (phase 7 evidence recorded in `docs/validation/`).
5. Every feature listed in the Rust `README.md` "Core behavior and boundaries"
   section has a corresponding implementation and a test.
6. No stubs remain in deliverable commands or supported operations. Missing host
   prerequisites produce explicit unverified acceptance items, not silent skips
   counted as successful live validation.
