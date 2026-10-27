# Architecture

This document describes the Go implementation: binaries, packages, runtime
behavior, concurrency and the data that flows between components. Field-level
compatibility rules are in [compatibility.md](compatibility.md).

## 1. Binaries

| Binary | Role | Default listener | State |
|---|---|---|---|
| `tt` | CLI client, image recipes, deployment | — | `~/.ttconfig` |
| `tt-ctl` | Controller: scheduling, lifecycle, fleet state, dashboard | `0.0.0.0:9200` | `<data-dir>/ctl.db`, `<data-dir>/service.lock` |
| `tt-agent` | Host-local VM/container lifecycle, storage, networking | `0.0.0.0:9100` | `<data-dir>/agent.db`, `<data-dir>/service.lock`, VM records |
| `upgrade-agent-v5` | Offline agent database converter (v5 → v6) | — | writes a new file only |

The two services and the CLI take `--api-key` (or `TT_API_KEY`). A service without
a configured key exposes an unauthenticated API and prints a warning. Keys must
be nonempty printable ASCII without whitespace; the offline converter has no key.

Startup order (both services):

1. Parse flags; validate the API key.
2. Create the data directory; take an exclusive non-blocking `flock` on
   `<data-dir>/service.lock` (mode 0600) for the process lifetime.
3. Open SQLite and run the service-specific schema gate/migrations and pragmas.
   Agent schema validation precedes WAL configuration; controller ordering follows
   its Rust migration path. See compatibility §4.
4. Resolve identity/configuration that is persisted in `_meta` (agent `host_id`,
   container runtime binding, network configuration).
5. Start background loops, bind the listener, serve with graceful shutdown on
   SIGINT/SIGTERM.

## 2. Package layout

```text
cmd/tt  cmd/tt-ctl  cmd/tt-agent  cmd/upgrade-agent-v5
model/         public domain and embedded wire types, enums, accounting, codecs
api/           public wire request/response types and the ApiResp envelope
internal/
  capability   design matrix, scoped host report, legacy tag mapping, admission
  backup       backup request validation, replay/preconditions, status mapping
  guestconfig  opaque guest files: validation, SHA-256 digest, ext4 config disk
  sshboot      SSH option resolution, seed script generation, readiness probes
  command      durable command receipts, receiver watermarks and forwarding context
  auth         API key parsing/compare and the bearer middleware
  runcmd       bounded command execution, output capture, polling helpers
  engine       Engine interface + factory, PID recovery/termination,
               qemu.go firecracker.go sandbox.go docker.go bhyve.go jail.go
  netcfg       bridge/TAP/NAT/DNAT (Linux), isolation, FreeBSD PF, TAP naming
  storage      ImageStore interface, file.go zvol.go, backup.go (artifacts),
               volume.go (independent disk/checkpoint primitives)
  agent        runtime.go (lifecycle), handler.go (HTTP), backup.go (worker),
               sshingress.go, volume.go/volume_binding.go/volume_http.go, config.go
  ctl          db.go, scheduler.go, handler.go, web.go (dashboard), backup.go
               (forwarding), volume*.go (admission/catalog/forwarding), config.go
  cli          client.go, root.go + per-command files, deploy.go, imagebuilder.go,
               ttconfig.go
```

Rules: `internal/` packages never import `cmd/`. Public `api` and `model` packages
do not import this module's `internal/` packages. Shared wire types and policy are
platform-independent; `guestconfig`, `sshboot` and `runcmd` also contain host-facing
helpers separated where necessary. `engine`, `netcfg` and `storage` are host-side and
select Linux or FreeBSD code by `runtime.GOOS` with per-OS files
(`*_linux.go`, `*_freebsd.go`, `*_other.go` returning "not supported on this
platform").

Go package imports must form this directed graph (arrows mean "imports"):

```text
model -> standard library, uuid (backup revision defaults)
api -> model
capability -> model
backup -> model
guestconfig -> model, capability, runcmd
sshboot -> model, capability, guestconfig, runcmd
auth / command -> api
engine / netcfg / storage -> shared packages (never agent / ctl / cli)
agent / ctl / cli -> shared packages (never each other)
cmd/* -> its component package
```

`model` owns embedded capability reports, backup state/request/generation and SSH
options/info as well as entities, enums and pure accounting methods. It imports
none of the other internal packages. `api.BackupView` embeds `model.Vm` and owns
the transport view. Policy functions in `backup` and `capability` accept model
values; they cannot be receiver methods added to types defined in another package.
Capability input adapters belong with callers: `capability` must not import
`api`/`sshboot` to resolve creation requirements. File-by-file Rust copying would
otherwise create `model ↔ backup`, `model ↔ capability` and SSH/config cycles.

Platform-neutral wire types and policy do not depend on host syscall packages.
Host operations in shared helpers use OS-specific files where necessary. The
suffix `_other.go` does not select an OS in Go: fallback files require explicit
`//go:build !linux && !freebsd` constraints. Cross-build all four binaries for
Linux/amd64 and FreeBSD/amd64; native execution remains platform-gated.

The guest-config disk builder's file/tool mechanics are private shared code so
its ext4 contents and publication can be tested on the dedicated FreeBSD build
host using e2fsprogs. The public `WriteDisk` entry point remains Linux-only;
passing these utility tests does not enable Firecracker on FreeBSD.

## 3. Data model

The entities (see [compatibility.md](compatibility.md) for exact JSON) are:

- `Host` — a registered agent: address, state, engines, storage backend, image
  catalog, legacy capability tags, optional scoped capability report, resource
  totals/reservations, last error.
- `Vm` — a workload: identity and environment, host, image, engine, cpu/mem/disk
  reservations, guest IP, `port_map` (guest → host TCP), retained creation
  `options` (ports, SSH keys/account, flags, requested disk, config digest),
  state, error, creation time, optional SSH metadata, optional pending resource
  target, and backup state.
- `Env` — a group of VMs with an owner label, creation and expiry timestamps and
  a lifecycle state.
- `Resource` — configured capacity and reservations (`cpu/mem/disk_total|used`,
  `vm_count`). Reservations, not measurements.

Accounting rules that shape the whole system:

- Stopped workloads keep their disk and slot reservations but release CPU and
  memory reservations.
- Firecracker reserves an extra 128 MiB of VMM headroom per running VM and 4 MiB
  for a configuration drive when one exists.
- Absence of a workload from an agent snapshot never silently releases
  reservations; the controller keeps it `failed` until reconciled.

## 4. Engines

`engine.Engine` is a small interface:

```go
type Engine interface {
    Create(ctx context.Context, vm *model.Vm, imagePath, diskFormat string, sshKeys []string) error
    Start(ctx context.Context, vm *model.Vm) error
    Stop(ctx context.Context, vm *model.Vm) error
    Destroy(ctx context.Context, vm *model.Vm) error
    State(ctx context.Context, vm *model.Vm) (model.VmState, error)
    Name() string
}
```

`engine.New(kind, containerRuntime)` returns the implementation for the current
platform; Docker requires the persisted container-runtime binding.

Shared host-side helpers used by all engines:

- `RecoverPID(pidPath, markers)` — never trusts a stale PID file. It validates
  `/proc/<pid>/cmdline` (Linux) or the `ps` listing (FreeBSD) against VM-specific
  markers, otherwise scans all processes for a unique match and fails on
  multiple matches ("resources retained"). A complete readable scan with no match
  returns absence, not an error; unreadable or ambiguous identity retains resources.
- `Terminate(pid, marker)` — SIGTERM, poll, SIGKILL, poll; a surviving process is
  an error, never a silent success.

Per engine (details mirrored from the Rust implementation):

- **QEMU**: `qemu-system-x86_64` with `-enable-kvm -daemonize`, virtio disk/network,
  MAC `02:54:aa:bb:cc:dd` (hex octets), PID file and an HMP text monitor socket in `RUN_DIR`, optional
  NoCloud seed ISO (`genisoimage`/`mkisofs`, volume id `cidata`) that carries
  static networking and either initial SSH provisioning or root keys. The engine's
  `Start` sends HMP `cont` to resume a paused process. Agent start of a stopped VM
  calls `Create` with the retained disk, without cloning it again. Stop sends
  `system_powerdown` with a 10 s grace then termination.
- **Firecracker**: per-VM clone directory with `vmlinux`, `rootfs.ext4` and an
  optional 4 MiB read-only `guest-config.ext4`; jailer chroot with a per-VM UID/GID
  derived from the guest IP (100000–165535), cgroup v2 limits (CPU quota, memory
  = RAM + 128 MiB, swap off, PID bound), entropy device, and a jailed TAP. Control
  over the Unix API socket (Go HTTP client); stop = `SendCtrlAltDel` with a 30 s
  grace on x86_64.
- **Docker/Podman**: `run -d --name tt-<id> --cpus … --memory … --memory-swap …
  -p host:guest …`; state via `inspect -f {{.State.Status}}`; the runtime binding
  is persisted on first use and never switches automatically.
- **bhyve** (FreeBSD 15.x): `bhyveload` + `bhyve`, `/dev/vmm/<id>`,
  process identity from `ps`, TAP device recorded in `RUN_DIR`.
- **jail** (FreeBSD 15.x): `jail -f <conf> -c`, shared IP stack with a
  `tt0` alias, devfs mount handling, root-key injection; no SSH bootstrap.

## 5. Storage

`storage.ImageStore` abstracts where base images and per-VM clones live:

- **file** — images are files (QEMU qcow2) or directories (Firecracker
  `vmlinux` + `rootfs.ext4`; Jail root trees). Clones are `cp --reflink=auto -R`
  copies into `<runtime-dir>/clone-<vm-id>`, with private permissions.
  `ResolveDisk` accepts a single unambiguous disk and rejects symlinks.
- **zvol** — images are ZFS volumes or datasets; clones come from a fixed
  `@ttsnap` snapshot (`zfs clone`), devices appear under `/dev/zvol/…`; resizing
  uses `zfs set volsize`. Firecracker bases keep the kernel in a dataset and the
  root filesystem in a child volume.

Filesystem growth helpers used by creation and resize: `qemu-img resize` for QEMU
images; for Firecracker, `set_len`/`zfs set volsize` followed by
`e2fsck -f -p` and `resize2fs` (always run, so an interrupted growth completes).

Backup artifacts (opt-in, stopped workloads only) use either ZFS snapshots named
`<disk>@ttbackup-<generation>` tagged with the user property
`ttstack:backup=<vm>:<generation>`, or strict Linux `FICLONE` reflinks under
`<runtime-dir>/.tt-backups/<vm>/<generation>/` with an `owner` file and a manifest.
There is no full-copy fallback.

## 6. Networking

Linux (QEMU/Firecracker via the shared bridge):

- Bridge `tt0` at `10.10.0.1/16`, forwarded IPv4. Guest addresses are allocated
  per host with `n = index + 1`, `10.10.<(n/254)&255>.<n%254+1>`;
  zero-based index 0 produces `10.10.0.2` (host-local, may repeat across hosts).
- TAP devices are deterministic: `tt-` plus 12 hex digits from a frozen
  SipHash-1-3 of the VM id (zero key, `0xff` terminator, digest masked to 48 bits).
  This is a host-visible identifier and must match the Rust implementation exactly.
- nftables table `tt-nat`: NAT masquerade for `10.10.0.0/16`, reply-direction
  accept, an egress denylist set, and per-port DNAT rules
  (`fib daddr type local tcp dport <host> dnat to <ip>:<guest>`). Per-VM isolation
  creates `bridge`/`inet` tables named after the TAP (dashes → underscores) that
  enforce MAC/IP/ARP consistency, block peer traffic, block guest-initiated host
  access, and drop private/link-local destinations while allowing public IPv4
  egress and reply traffic.
- Container port publishing is delegated to Docker/Podman.
- Optional SSH ingress: an outer-netns DNAT rule (`nsenter` + `nft` table
  `ip tt-ssh`) that exposes only the VM's mapped SSH port.

FreeBSD: `ifconfig` bridge/taps with ownership recorded as interface
descriptions, plus PF anchors under `ttstack/` (preflight checks the operator's
`rdr-anchor`/`anchor` hooks; TTstack never edits the root ruleset).

## 7. SSH bootstrap and readiness

For QEMU and prepared Firecracker images the agent generates a per-VM Ed25519 host
identity and a seed script (`RUN_DIR/ssh-{id}.sh`). QEMU embeds it in cloud-init
user-data; Firecracker exposes it on the read-only configuration drive, and the
prepared image runs it through `/etc/ttstack/ssh-bootstrap-version == 1`
support. The script creates the account, installs keys, writes the host key and a
key-only `sshd_config`, optionally grants passwordless sudo, and records
`/var/lib/ttstack/ssh-initialized`. It runs once; restarts never rewrite caller
changes. Jail injects root keys directly. Readiness is a TCP banner check plus,
for the first confirmation, an `ssh-keyscan` comparison against the generated
identity.

## 8. Backup protocol

One published recovery point per VM, with durable operations:

- Controller and agent expose `GET/PUT/DELETE /api/vms/{id}/backup` and
  `POST /api/vms/{id}/backup/restore`.
- Mutations require `Idempotency-Key: <operation uuid>` and
  `If-Match: "<revision uuid>"`. `backup.Check` resolves replays (a completed
  identical operation returns its receipt without touching storage), conflicts
  (same operation id, different parameters), unfinished operations and stale
  revisions before any work starts.
- The agent persists intent (pending request + artifact identity) before touching
  storage, performs snapshot/reflink work with a file lock inherited by any
  surviving storage child, then publishes. A refresh keeps the previous generation
  until the new one is verified, retires the old one, and cleans retired artifacts
  asynchronously with backoff; at most two retired generations are retained.
- Restore is destructive, leaves the VM stopped, requires matching capacity and
  boot dependencies, and invalidates SSH observations.
- The controller forwards mutations with the same headers, reserves disk for
  in-flight work, and reconciles unknown outcomes from agent snapshots.

## 9. Controller

- **Database**: `<data-dir>/ctl.db`, schema v9 (forward migration from v5). Tables `_meta`, `hosts`, `envs`,
  `vms` (id + JSON blob, plus `env_id`/`host_id` columns and indices). A newer
  schema refuses to open; the v5 migration materializes a stable backup revision
  per VM.
- **Scheduler**: eligibility (online, engine present, capability admission, image
  available, disk reservation resolvable, resources fit) then preference: zvol
  hosts first for VM engines, then least free memory (best fit). An environment is
  scheduled VM by VM against a shadow copy of host reservations and fails as a
  whole if any VM cannot be placed.
- **Lifecycle**: creation persists the full plan before contacting agents and
  returns HTTP 202; a spawned worker creates VMs sequentially and reconciles with
  a scoped host refresh. Deletion persists `deleting` first and retries; a 60 s
  expiry loop retries incomplete deletions and expired environments with
  concurrency 8. Start/stop mutate agent state, then confirm locally and via a
  scoped refresh.
- **Refresh and reconciliation**: a 15 s loop reads agent `/api/info` for the
  whole fleet in batches of 8 with a 5 s timeout, applying snapshots only if the
  controller revision did not change meanwhile; absent VMs are retained as
  `failed`, unknown VMs are counted and logged, never adopted. Per-environment
  operations serialize through keyed mutexes; conflicts return HTTP 409.
- **Offline resource updates**: the controller persists the target as pending
  intent before forwarding, validates capability/state/backup/disk-growth rules,
  and keeps intent across unknown outcomes until a snapshot confirms it.
- **Dashboard**: a single embedded HTML page with vanilla JS at `/`, public while
  `/api/*` requires the bearer key; it drives the same endpoints as the CLI and
  uses the capability matrix to enable/disable creation fields.

## 10. Agent

- **Runtime**: one mutex-guarded runtime object owns the database, engine
  instances and in-flight operations; HTTP mutations are spawned so they survive
  client disconnects. Reads use a separate read-only database handle.
- **Lifecycle**: creation validates everything (names, image, options, capability
  admission, resources, MAX_VMS), allocates IP/ports, saves a `creating` record
  before side effects, prepares SSH, network and disks, then runs the engine and
  records `running`/`failed`. `start` cold-boots retained disks, `stop` shuts
  down gracefully with engine-specific fallbacks, `destroy` stops, removes backup
  artifacts, tears down networking and deletes the clone.
- **Reconciliation**: a 15 s loop converges recorded state with the engine,
  retries interrupted deletions and pending backup work, restores missing
  network rules, and re-observes SSH readiness.
- **Image catalog**: a 15 s background refresh of the image directory; a slow
  scan keeps the previous catalog and reports a warning.
- **Corruption policy**: unreadable VM rows are never deleted; they disable
  admission and pin reservations to capacity so nothing is silently released.

## 11. Concurrency and locks

| Scope | Mechanism |
|---|---|
| Controller per-environment mutation | keyed mutex map + operation revision counter; conflicting requests → 409 |
| Controller snapshot application | revision counter read before a scan; stale snapshots are dropped |
| Controller expiry | semaphore of 8 |
| Agent lifecycle mutations | single runtime mutex; long work released for backup/cleanup paths |
| Agent backup writer | one writer per VM, bounded workers, `flock` file inherited by storage children |
| Service state directory | exclusive `flock` on `service.lock` for the process lifetime |
| SQLite | WAL, `synchronous=FULL`, `busy_timeout=5s`, single writer discipline |

The SQLite writer uses one pinned connection initialized with the exact service
pragmas. All writes and transactions use that connection under component locking;
never call the pool from inside its transaction and wait for the same connection.
Agent snapshot readers use separately initialized read-only connections with the
Rust 2 s busy timeout. Resource totals and VM rows come from one snapshot; reader
access must remain possible while a lifecycle operation holds the runtime lock.
If a connection is replaced, reapply its connection-local pragmas before use.

Accepted mutations use service-owned operation contexts with bounded tool/network
deadlines, not the HTTP request context: a client disconnect cannot cancel durable
work and release its reservations. Shutdown stops admission and background polling,
drains active HTTP handlers, then cancels detached controller creation/expiry waits
without erasing their durable intent. All controller goroutines exit before the
state lock is released. Agent accepted-operation draining is unchanged. Do not
hold controller database locks over agent HTTP calls. Tests cover disconnect,
shutdown, stale refresh and lock ordering with `go test -race`.

Command capture mirrors Rust's temporary-file strategy: read at most 4 MiB from
each stream after the child exits, with a 300 s default timeout. This bounds
returned output, not temporary disk usage. Reference CLI calls without a deadline
use the explicit unbounded entry point; this does not disable engine/storage
command budgets. Avoid pipe readers that wait forever
when descendants inherit stdout/stderr. Timeout termination must not blindly kill
an entire process group containing a launched guest. Backup storage children
explicitly inherit only their writer lock, using `ExtraFiles` or the corresponding
platform mechanism, so a surviving child prevents a second writer after timeout.

## 12. Error handling and HTTP mapping

Handlers return `(status, ApiResp[T])`. Errors are plain Go errors flattened to
strings at the boundary; status selection mirrors the Rust controller:
400 validation/design/capability, 404 missing, 409 conflicts/busy/duplicate/limit,
412 stale backup revision, 422 unschedulable placement, 428 missing backup
precondition headers, 502 agent failure/unknown outcome, 503 offline host,
202 accepted creation. The backup protocol additionally maps message prefixes
(`precondition:`, `missing precondition:`, `not found:`, `conflict:`,
`backup unsupported:`, `invalid …`) to statuses in one shared table.

## 13. Deployment topology

Fixed defaults for local deployment: prefix `/opt/ttstack`, service user
`ttstack` (controller) and root (agent), systemd units `tt-ctl.service` /
`tt-agent.service` with `KillMode=process` and a 360 s stop timeout for the agent,
environment files `/opt/ttstack/etc/tt-{ctl,agent}.env`, and the protected shared
key at `/opt/ttstack/etc/api-key` (0600). Runtime paths: `/home/ttstack/images`,
`/home/ttstack/runtime`, `/home/ttstack/data/agent.db`, `/home/ttstack/ctl/ctl.db`
and the fixed engine run directory `/home/ttstack/run`. Distributed deployment
reads `deploy.toml`, resolves one shared key (config → sidecar `.api-key` →
existing service env → generated), and applies per-host scripts over SSH stdin
with SHA-256 verification of uploaded binaries; systemd, OpenRC and an unmanaged
fallback are supported.

## 14. Testing architecture

- `internal/*/testdata` holds synthetic JSON/SQL fixtures generated from the pinned
  Rust implementation. Database files are created in temporary directories.
  JSON comparison is semantic and number-preserving; bytes are compared for
  contractual text and digest input. Negative decoding fixtures are required too.
- Contract tests ported from Rust unit tests cover scheduler placement, capability
  admission, backup replay, validation and TAP-name vectors.
- `httptest`-based workflow tests exercise controller↔agent flows with mock agents
  (timeouts, partial failure, expiry, detach, resize recovery).
- Engine tests use fake `PATH` executables to assert exact command lines.
- FreeBSD-only code is cross-compiled in Linux CI and reviewed against the source;
  native tests are separate evidence and cannot be inferred from a cross-build.
