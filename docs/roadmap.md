# Roadmap

Phases 0–7 below record the original delivery and its acceptance. Ongoing
maintenance follows [the AI workflow](ai-workflow.md), with atomic per-issue
commits when requested; it does not reopen a phase or trigger a release bump.

Phased delivery of the Go port. Every phase ends with a green build, `go vet`,
`go test ./...`, and exactly one commit. Later phases must not start before the
previous phase's acceptance criteria are met — the point of the staging is that a
failure is always localized.

Reference sources for each phase are under `../TTstack/crates`, pinned to the
revision in [plan.md](plan.md). Plan-review corrections are committed separately
before phase 0. Completed phases end with one acceptance commit; do not combine
multiple phases or mark a phase complete with missing acceptance work.

## Acceptance rules used by every phase

- Required local gates: `gofmt -l .`, `go vet ./...`, `go test ./...`,
  `go build ./...`. From phase 0, cross-build Linux/amd64 and FreeBSD/amd64;
  from phase 1, run `go test -race ./...` on a suitable authorized test host.
  This workstation is shared and constrained: use `GOMAXPROCS=2` and Go `-p 1`
  for local checks, sequentially. No local VMM, container daemon, bridge, firewall,
  namespace setup or other real virtualization infrastructure is permitted.
  Heavy Rust builds, race/integration suites and native tests run remotely, in
  the FreeBSD-first order below; phase completion includes those remote gates.
- Normal Go tests use committed synthetic fixtures and fake commands, without
  needing the sibling Rust repository, root, network access or a VM host.
  CI installs ext4/SSH tools and explicitly enables those integration tests;
  developer runs keep integration tests opt-in. Missing prerequisites fail an
  explicitly requested integration suite rather than silently passing it.
- The Rust harness records its source revision, Cargo lock hash and regeneration
  command. Differential tests use actual Rust decoding as well as serialization.
  CLI, HTTP and state comparison begins in the phase that introduces each surface;
  phase 7 assembles evidence instead of first discovering protocol mismatches.
- Phase gates are cumulative. FreeBSD cross-compilation is not native validation;
  a skipped reflink, ZFS, engine or guest test is not a successful acceptance case.

## Live validation order and isolation

Use local tests first, then the authorized dedicated FreeBSD test host for early
service, state, CLI and available bhyve/Jail lifecycle checks. Only after these
flows are stable add the authorized Linux hosts for wider engine/backend checks.
The dedicated FreeBSD host has no CPU fraction restriction under the updated
operator authorization; keep guest memory conservative and monitor headroom to
avoid OOM. The half-capacity rule below still applies to both shared Linux hosts.
Private access details remain outside the repository. Native FreeBSD capability
limits remain explicit even when tests pass.

On Linux, inspect existing load and leave at least half of each host's CPU and
memory available, including existing workloads in the budget. Place agents, guest
networking and firewall operations in task-owned network namespaces. Because
`RUN_DIR`, bridges and firewall names are fixed, also isolate filesystem/runtime
paths (mount namespaces or an equivalent isolated root), with unique VM IDs and
separate service state. Distinct TCP ports alone do not isolate these resources.
Expose management through controlled SSH access; never reset the root firewall,
change the management route or replace existing services. Record created resources,
verify SSH reachability before/after changes, and clean only those resources.
Check isolation preconditions before launching an agent, whose startup may modify
host networking. Linux-only engines still need Linux validation after FreeBSD.

## Phase 0 — Repository skeleton

Status: complete. Local Go 1.26.0 and 1.27.1 build/vet/test and CGo-disabled
Linux/FreeBSD cross-build gates pass. All four help entries execute. Remote CI
has not been run; the skeleton intentionally refuses functional operations.

Deliverables

- `go.mod` (module `github.com/aisidev/tt`, Go 1.26 minimum) with the four direct
  dependencies and their required indirect modules/checksums. Pin versions that
  build with the minimum toolchain; verify it separately from the local toolchain.
- `Makefile`: `build`, `release`, `test`, `lint`, `fmt`, `clean`, `install`,
  `deploy-agent`, `deploy-ctl`, `deploy`, `help`.
- `.github/workflows/ci.yml`: the phase gates on the minimum Go toolchain, with
  `e2fsprogs` and OpenSSH tools installed, and Linux/FreeBSD cross-build jobs.
- `cmd/{tt,tt-ctl,tt-agent,upgrade-agent-v5}/main.go` stubs that parse flags and
  print usage.
- `AGENTS.md`, `docs/` (this set). The original `CLAUDE.md` forwarding file was
  retired when maintenance standardized on `AGENTS.md` and `.agents/skills/`.

Acceptance

- `go build ./...`, `go vet ./...`, `go test ./...` clean.
- `go run ./cmd/tt --help` and the two service binaries with `--help` print the
  documented flags.

## Phase 1 — Shared core

Status: complete. [Validation evidence](validation/shared-core-2026-09-30.md)
records 1,642 Rust cases, minimum-toolchain gates and native FreeBSD race/integration
checks. Linux engine and database acceptance remains in its owning later phases.

Deliverables (`model`, `api`, `capability`, `backup`, `guestconfig`,
`sshboot`, `auth`, `runcmd`)

- Entities, enums (strict JSON), constants, resource accounting, validation.
- Wire DTOs and the `ApiResp` envelope with exact omission rules.
- Capability design matrix, scoped `Report` admission, legacy tag mapping.
- Backup protocol types with replay/precondition logic and the status table.
- Guest configuration validation, digest, ext4 config-disk builder.
- SSH options/info, seed-script generation, the embedded bootstrap script,
  readiness probes.
- API key parsing/comparison and bearer middleware.
- Bounded command execution.

Acceptance

- Rust-generated fixtures for every wire type pass semantic round trips and
  positive/negative decoding cases (presence, null, defaults, duplicate fields,
  exact field names, enums, integer bounds and strict nested objects).
- Byte-exact guest-config digest vectors cover escaping and Unicode. Empty
  collections must emit the Rust shape; defaults must survive persistence reads.
- The implemented package imports match the acyclic architecture graph.
- Ported unit tests for validation, capability admission, backup replay,
  resource accounting and the envelope pass.
- `mkfs.ext4`/`debugfs` based config-disk test passes (guarded by tool presence).
- `sh -n` check of the generated seed script with hostile key material.
- Command timeout tests cover inherited output descriptors and retained backup
  writer locks, not just a direct `sleep` child.

## Phase 2 — Storage and networking

Status: complete. [Validation evidence](validation/storage-network-2026-09-30.md)
records local gates, Rust rule fixtures, native FreeBSD race/ext4/ZFS checks and
cleanup. Linux kernel networking and successful reflink recovery remain in the
phase-7 native matrix; the unsupported-reflink contract is exercised on FreeBSD.

Deliverables (`internal/storage`, `internal/netcfg`)

- `ImageStore` with `file` and `zvol` backends: clone, remove, list, resolve,
  sizes, resize, Firecracker directory/layout handling, ext4 growth.
- Backup artifact storage: zvol snapshots with ownership properties, reflink
  artifacts with owner/manifest files, verification, restore staging, cleanup.
- Linux networking: bridge/TAP lifecycle, nftables NAT/DNAT/denylist, per-VM
  isolation, TAP owner changes, SSH ingress rules.
- FreeBSD networking: `ifconfig` bridge/taps with ownership descriptions, PF
  anchor preflight and rules.

Acceptance

- TAP-name, MAC and IP-allocation vector tests pass.
- Reflink probe tests (skip when the filesystem cannot reflink) and zvol command
  golden tests pass.
- Isolation rule text golden tests pass; FreeBSD code cross-compiles. Unit tests
  must never modify the developer host's real firewall or bridge.

## Phase 3 — Engines

Status: complete. [Validation evidence](validation/engines-2026-09-30.md) records
Rust command/configuration fixtures, local fake-tool lifecycle tests and native
FreeBSD race/integration gates. Block-node creation is verified at the syscall
boundary; real Linux device creation and guest boot remain in phase 7. No native
VM/container lifecycle is claimed by the fake-tool results.

Deliverables (`internal/engine`)

- `Engine` interface, platform factory, PID recovery and termination helpers.
- QEMU: command construction, NoCloud seed ISO, HMP text monitor, lifecycle.
- Firecracker: config JSON, jailer sandbox, cgroup v2, configuration drive,
  shutdown escalation, `Sandbox` metadata.
- Docker/Podman: run/inspect/start/stop/rm, persisted runtime binding.
- bhyve and jail: FreeBSD lifecycle with process/device ownership checks.

Acceptance

- Command-line golden tests with fake `PATH` executables for every engine.
- Sandbox tests: hard links preserved, block devices recreated, symlinks rejected.
- Boot-wait semantics tests (missing socket ≠ exited process, timeout retains
  resources) pass.
- PID recovery covers a clean zero-match scan, unreadable identity, duplicate
  matches and PID reuse. Stop/start tests distinguish cold boot from pause/resume.

## Phase 4 — Agent

Status: complete. [Validation evidence](validation/agent-2026-09-30.md) records
native Rust/Go database and converter interchange, 174 HTTP cases, lifecycle and
backup fault/retry tests, FreeBSD race/integration gates, and Go→Rust→Go agent
service takeover against retained synthetic inventory. Real guests and the
controller/CLI workflow remain later native acceptance work.

Deliverables (`internal/agent`, `cmd/tt-agent`)

- Runtime: create/start/stop/resize/destroy/reconcile with durable intent,
  idempotent creation, IP/port allocation, SSH bootstrap, network restore.
- HTTP handlers with the exact routes and status mapping.
- Backup worker: claim/admit/finish, cleanup with backoff, settle headers.
- SSH ingress application/removal.
- Agent database v6 with the corruption policy.
- `cmd/upgrade-agent-v5`: offline converter preserving unknown JSON fields.

Acceptance

- `httptest` tests for create idempotency, capability rejections, resize
  conflicts and backup header/status behavior.
- Schema gate tests: v5 and garbage databases are rejected, v6 opens.
- Converter tests: source unchanged, output mode 0600, unknown fields preserved.
- Actual Rust-created agent databases open in Go and Go-written rows reopen in
  Rust; temporary files stay outside git. Test empty-but-versioned and damaged v6
  databases, not only populated v5 inventories.
- Rust/Go differential HTTP tests cover auth, extractor and router edge cases,
  including disconnect-surviving mutations and settled backup response headers.
- Native FreeBSD service/state smoke tests precede any early Linux live testing.

## Phase 5 — Controller

Status: complete. [Validation evidence](validation/controller-2026-09-30.md)
records v5 database/migration interchange, scheduler and failure/retry workflows,
52 native HTTP cases, exact dashboard content, shutdown draining and all four
Go/Rust controller-agent service pairings on FreeBSD with synthetic inventory.
CLI and real guest acceptance remain in phases 6–7.

Deliverables (`internal/ctl`, `cmd/tt-ctl`)

- Database v5 with migration and recovery read semantics.
- Scheduler with the exact eligibility and preference rules and error messages.
- Handlers: hosts, environments, VMs, resize, images, status, capabilities,
  detach, and the backup forwarding path.
- Lifecycle: plan-persist-then-execute creation, deletion retry, expiry loop,
  start/stop with reservation handling, snapshot merge rules.
- Dashboard (embedded HTML/JS) driven by the capability matrix.

Acceptance

- Mock-agent workflow tests pass: interrupted creation, partial deletion, expiry
  retry, duplicate names, unschedulable requests, resize unknown outcomes,
  absent-VM retention, untracked-VM logging.
- Synthetic SQL/JSON fixture reconstruction passes; actual Rust-created controller
  databases open in Go, and Go mutations reopen in Rust, including migration rollback.
- Dashboard serves at `/` and its API calls work against the test server.
- Unknown paths do not fall through to the dashboard; HTTP differential cases pass.
- Concurrent mutation/refresh, client disconnect and shutdown/recovery tests pass
  under the race detector without silently releasing reservations.

## Phase 6 — CLI, deployment, images

Status: complete. [Validation evidence](validation/cli-2026-09-30.md) records
24 Rust CLI output/request cases, 32 help entries, byte-exact deployment templates,
fake deployment/image failure tests, minimum-toolchain gates and FreeBSD native
race/integration plus both CLIs against all four controller-agent pairings. Real
image builds, deployment to Linux services and guest boot remain in phase 7.

Deliverables (`internal/cli`, `cmd/tt`)

- Client with redirects disabled, per-request timeouts and no implicit retries.
- Command tree: `config`, `capabilities`, `status`, `host`, `env` (including
  `resize` and `backup`), `image`, `deploy`.
- `~/.ttconfig` handling (atomic write, mode 0600, address-scoped credentials).
- Local deployment (systemd units, env files, key lifecycle, mount-guard
  drop-ins) and distributed deployment (`deploy.toml`, SSH stdin scripts, SHA-256
  verified uploads, systemd/OpenRC/manual fallback).
- Image recipes with pinned digests for `alpine-cloud` and `fc-alpine`, checksum
  manifest verification for Debian/Ubuntu, Docker pulls with local aliases, and
  the FreeBSD jail base recipe.

Acceptance

- CLI golden output tests for tables, expiry labels, access lines and errors.
- Deploy script tests with a fake root/SSH environment; unit files match the
  documented content.
- `tt image recipes` and recipe command-plan tests with fake HTTP/tools pass,
  together with checksum/publication tests. Do not add a public `--dry-run` option:
  the reference CLI has none.
- Linux systemd/deploy scripts are checked locally using fakes before Linux live
  use. Minimum-toolchain builds and FreeBSD binaries still pass. Native
  FreeBSD validation is FreeBSD 15.x only.

## Phase 7 — Compatibility acceptance

Status: complete. [FreeBSD evidence](validation/freebsd-native-2026-09-30.md)
records real Jail/bhyve lifecycle, live implementation swaps and bhyve zvol recovery.
[Linux evidence](validation/linux-native-2026-09-30.md) records QEMU/Firecracker on
file/zvol, real SSH and resource growth, replay and held-snapshot recovery, a mixed
fleet with isolated networking/outer SSH ingress, private Docker and systemd
workflows. [Feature coverage](validation/feature-coverage.md) maps every pinned
README boundary to tests. External-runtime/platform limits are explicit in those
reports; no fleet-capacity or stress-test claim is made.

Deliverables

- Cross-implementation tests: Go controller ↔ Rust agent, Rust controller ↔ Go
  agent, Go CLI against both.
- Feature checklist walk-through of the Rust `README.md` "Core behavior and
  boundaries" section, each item mapped to a test.
- `docs/validation/` record of the tested revisions, hosts and limits.
- Final documentation pass for guides introduced with their owning phase:
  `deployment.md`, `rest-api.md`, `guest-images.md`, `ssh.md`, `disk-backup.md`.

Acceptance

- After FreeBSD flow validation, run the bounded Linux matrix below with isolated
  services. A mixed environment spans separate Go and Rust agents; one controller
  manages it at a time. Exercise both controller implementations and both CLIs.
- Every README core behavior maps to an implementation and named test/evidence.
  Every documented command is exercised with fakes or an eligible native host;
  record which. Documentation links resolve. Missing native prerequisites remain
  explicit outstanding acceptance items, never represented as a complete port.

| Native surface | Required observations |
|---|---|
| Dedicated FreeBSD first | Service startup/restart, schema/identity persistence, controller/CLI workflow; available bhyve and Jail create/stop/start/delete; eligible bhyve zvol backup if prerequisites exist |
| Linux QEMU, both controller/agent pairings | Create, SSH readiness, retained marker after stop/start, offline resize, delete; file and zvol coverage |
| Linux Docker/Podman | Create, published port, stop/start/delete; persisted runtime binding and refusal to silently switch stores |
| Linux Firecracker | Jailer/cgroup startup, opaque config/digest, SSH on a prepared image, retained disk, resize and restart recovery; file and zvol coverage |
| Eligible backup combinations | Strict reflink and zvol creation/refresh/restore/delete, retained previous generation, exact replay, unknown outcome and cleanup recovery |
| Network and process recovery | Isolated egress/ingress cases, missing/stale PID metadata, agent/controller restart, no changes to pre-existing host networking/services |
