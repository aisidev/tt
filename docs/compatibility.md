# Compatibility contract with the Rust implementation

This document is normative for the Go port. Where the Go code and this document
disagree, one of them is a bug. Reference paths point into `../TTstack`.
The oracle is revision `1ad83a9feed5fac820d74b5e7ef752e50a552740` (0.6.0),
as pinned in [plan.md](plan.md). Rust source wins over an incorrect plan statement
for the parity surface; explicitly authorized extensions in §10–15 have their own
contracts and forward-only schema versions.

## 1. Scope

Three surfaces must match exactly:

1. **HTTP/JSON** — routes, methods, request and response documents, status codes.
2. **Persistent state** — SQLite files, schema versions, `_meta` keys, on-disk
   runtime artifacts.
3. **Host-visible identifiers** — names shared with the kernel, the init system or
   the filesystem that must survive a binary swap.

The CLI's printed output and exit codes are part of the contract as well, because
operators and scripts depend on them.

## 2. JSON rules (serde → Go)

serde has behaviors that Go's `encoding/json` does not reproduce by default.
Every wire struct must follow them:

| serde attribute | Meaning | Go implementation |
|---|---|---|
| `#[serde(default)]` on a field | Missing input uses its default; explicit null is not a missing non-optional field | Presence-aware decoding; use the Rust default, not automatically Go's zero value |
| `skip_serializing_if = "Option::is_none"` | Omitted from output when absent | `json:"name,omitempty"` on a pointer field |
| `skip_serializing_if = "Vec::is_empty"` / `"BTreeMap::is_empty"` | Omitted from output when empty | `json:"name,omitempty"` on a slice/map field |
| none of the above on `Option<T>` | Field is **always present**, `null` when absent | pointer field **without** `omitempty` |
| none of the above on `Vec`/`BTreeMap` | Always present, `[]`/`{}` when empty; null is invalid input | Non-nil empty slice/map plus explicit null checks, **without** `omitempty` |
| non-optional field without a default | Required on input, even when its zero value is meaningful | Track presence; ordinary Go decoding silently accepts missing fields |
| `deny_unknown_fields` | Unknown input keys are an error | Enforce in that type's codec, including nested objects and database decoding |
| `rename_all = "lowercase"` / `"snake_case"` | Enum/label spelling | custom `MarshalJSON`/`UnmarshalJSON` per enum |

Fields that are **always present and nullable** today (they must not gain
`omitempty`): `Host.error`, `Env.error`, `Vm.error`, `Pending.error`,
`Receipt.error`, `Status.reason`, `View.unsupported_reason`, `Request.generation`,
the optional fields of `backup.State`, and `VmSpec.cpu/mem/disk`.
`Generation` has no optional fields: all its scalars are required and non-null.
Fields that are **omitted when absent**: `Host.capability_report`, `Vm.ssh`,
`Vm.pending_resources`, `VmOptions.ssh`, `VmOptions.guest_config_digest`,
`SshInfo.observation_error`, `AgentInfo.capability_report`, `AgentInfo.vms`,
`ApiResp.data`, `ApiResp.error`, and every `Vec`/map that has an emptiness rule
(`warnings`, request `ssh_keys`, `guest_config`, `evidence`). In contrast,
`VmOptions.ssh_keys`, `VmOptions.ports` and `State.retired` are always present.
When in doubt, read the field's attributes in `crates/core/src/model.rs`,
`api.rs`, `capability.rs`, `backup.rs` and `ssh.rs`; those files are the source of
truth, and each wire type gets a golden fixture in this repository.

Codecs must also preserve exact field-name matching and duplicate-known-field
rejection. Go's default case-insensitive field matching, last-key-wins decoding,
null-to-zero handling and invalid-UTF-8 replacement for decoded strings are not
serde parity. Unknown
fields remain allowed on types without `deny_unknown_fields`; do not globally
enable strict rejection. HTTP bodies must contain exactly one JSON value.
Rust skips the values of unknown fields rather than decoding them as typed
strings; differential fixtures include ignored unpaired-surrogate escapes.

serde also accepts positional arrays for these derived structs. Their positions
follow Rust declaration order, including fields omitted by serialization. Missing
trailing fields use only explicit defaults; an `Option` field without a default
must occupy its position, even though it may be absent in object input. Output
remains an object. Positive and negative sequence fixtures fix this behavior.

Examples of non-zero defaults: absent `VmSpec.engine` means `qemu`; absent
`Vm.backup` creates a complete backup state with a fresh UUID revision. A present
`backup` must itself decode correctly and is not repaired by defaults. A wholly
absent `Vm.options` defaults as a unit, while a present options object still has
required fields. Apply these rules in persistence as well as HTTP codecs.

### Enums

JSON uses **canonical lowercase variant names only**; aliases exist only in CLI
parsing. Serde unit enums also accept an externally tagged object with exactly one
variant key and a null value, for example `{"qemu":null}`. Go accepts the same
input and always serializes it as the canonical string. Non-null values, extra
keys and duplicate variant keys remain invalid.
CLI case folding is ASCII-only, matching Rust `to_ascii_lowercase`; Unicode lookalikes
are not aliases.

| Type | JSON values | CLI aliases (not JSON) |
|---|---|---|
| `Engine` | `qemu`, `firecracker`, `bhyve`, `docker`, `jail` | `kvm`→qemu, `fc`→firecracker, `podman`→docker, case-insensitive |
| `Storage` | `file`, `zvol` | case-insensitive |
| `VmState` | `running`, `stopped`, `paused`, `creating`, `failed`, `deleting` | — |
| `EnvState` | `active`, `stopped`, `creating`, `deleting`, `failed` | — |
| `HostState` | `online`, `offline` | — |
| `Backend` | `zvol`, `reflink` | — |
| `backup.Action` | `create`, `restore`, `delete` | — |

An unknown value is a decode error — never a fallback. This matters for persisted
records: a row containing an unknown engine is *unreadable*, not upgradable, and
the strict reads (controller `get`/`list`) must fail while recovery reads skip the
row with a log line.

### Numbers and maps

- Timestamps are Unix seconds as unsigned 64-bit integers; `expires_at: 0` means
  "never".
- MiB units for memory and disk; ports are unsigned 16-bit.
- `port_map` is a JSON object (`{"22":20000}`) mapping guest port → host port.
  Use `map[uint16]uint16` and a compatible key decoder: Rust parses keys as u16,
  so arbitrary strings, leading zeroes, escaped digits and out-of-range values
  are invalid. Ordering is irrelevant
  to JSON semantics; CLI/rule construction must explicitly sort numeric ports.
- Saturating arithmetic is preserved everywhere reservations are computed.

Compare JSON fixtures structurally with integer-preserving decoding (`UseNumber`
or typed values), never through float64. Object order and equivalent JSON escapes
are not wire differences; missing, null, empty array and empty object are distinct.
Guest-config SHA-256 is an exception: its input is exactly serde_json's compact
UTF-8 serialization of the sorted string map. Go's default HTML escaping, even
`SetEscapeHTML(false)` for U+2028/U+2029, and Encoder's final newline need explicit
handling. Rust-generated digest vectors must include `<`, `>`, `&`, Unicode line
separators, control escapes and multibyte text.

## 3. HTTP surface

Both services answer with the envelope `{"ok":true,"data":…}` or
`{"ok":false,"error":"…"}`; `data`/`error` are omitted when absent; an empty
success through `ApiRespEmpty::ok()` is exactly `{"ok":true}`. Controller
environment delete/start/stop use `ApiResp::success(())` and include `"data":null`. Malformed JSON may produce a non-envelope body
(same as the framework behavior in Rust); clients check both the status and `ok`.

Controller routes (`crates/ctl/src/main.rs`):

| Method(s) | Path | Success |
|---|---|---|
| GET | `/` | dashboard HTML (public) |
| GET | `/api/capabilities` | capability `Matrix` |
| GET, POST | `/api/hosts` | `Host[]`; POST → 201 `Host` (200 on repeat) |
| GET, DELETE | `/api/hosts/{id}` | `Host`; DELETE → empty |
| POST | `/api/hosts/{id}/detach` | orphan `Vm[]` |
| GET, POST | `/api/envs` | `Env[]`; POST → 202 `EnvDetail` |
| GET, DELETE | `/api/envs/{id}` | `EnvDetail`; DELETE → empty |
| POST | `/api/envs/{id}/stop`, `/start` | empty |
| GET | `/api/vms/{id}` | `Vm` |
| POST | `/api/vms/{id}/resources` | updated stopped `Vm` |
| GET, PUT, DELETE | `/api/vms/{id}/backup` | `View` (+ `ETag: "<revision>"`) |
| POST | `/api/vms/{id}/backup/restore` | `View` |
| GET | `/api/images` | `ImageInfo[]` (online hosts) |
| GET | `/api/status` | `FleetStatus` |

Agent routes (`crates/agent/src/main.rs`): `GET /api/info`, `GET /api/images`,
`GET|POST /api/vms`, `GET|DELETE /api/vms/{id}`, `POST /api/vms/{id}/stop|start`,
`POST /api/vms/{id}/resources`, and the four backup routes.

Status overview (endpoint-specific, not one global error classifier):

| Condition | Status |
|---|---|
| Validation, design/capability rejection, invalid resize values, unsupported backup | 400 |
| Missing entity (`not found:`) | 404 |
| Busy environment, duplicate name, limits, conflicts, insufficient capacity on resize | 409 |
| Stale backup revision (`precondition:`) | 412 |
| Unschedulable placement | 422 |
| Missing backup precondition headers (`missing precondition:`) | 428 |
| Agent failure, unknown outcome, incomplete cleanup | 502 |
| Offline host during a mutation that needs it | 503 |
| Creation accepted | 202 |
| Agent creation capability rejection | 400 (agent) |
| Other agent create/start/stop/delete errors | 500 (agent) |

Backup UUIDs accept the Rust parser's hyphenated, simple-hex, brace-wrapped and
lowercase `urn:uuid:` forms. Hex digits may use either case; arbitrary wrappers
and uppercase URN prefixes are invalid. Validate this explicitly: Go's UUID parser
is more permissive for wrappers and URN prefix casing.

Backup mutations additionally carry `Idempotency-Key` (UUID) and `If-Match`
(quoted revision UUID), and the agent answers with `X-TT-Backup-Settled` /
`X-TT-Backup-Settled-Sequence` headers on failure when the writer has settled.
The message-prefix → status table lives in `crates/core/src/backup.rs`
(`error_status`) and is mirrored in `internal/backup`; its fallback is **500**.
Controller forwarding separately maps upstream 5xx, transport failure and invalid
responses to **502**, preserving upstream client errors. Other lifecycle endpoints
use their own status rules; do not apply the backup prefixes globally.

Authentication: `Authorization: Bearer <key>`, split at the first literal space,
scheme compared case-insensitively,
token compared in constant time (length is not hidden). Missing/invalid → 401 with
the standard envelope and `invalid or missing API key`. Do not trim the token or
normalize extra whitespace. Configured keys must be nonempty ASCII 0x21–0x7e.
No key configured → no authentication at all. There is no
per-owner or per-environment authorization anywhere.

Router/extractor behavior needs differential HTTP tests: HEAD, unsupported methods,
unknown routes, slash/escaped-path handling, JSON content type, size limits,
malformed/trailing JSON, and middleware order. `net/http.ServeMux` alone is not an
Axum compatibility layer; use an exact dashboard route (`GET /{$}`) instead of
letting `/` catch every unknown path. Capture Rust rejection status, headers and
body before choosing Go validation or HTTP server limits.

## 4. Persistent state

Before v1.0, maintain only the latest development schema under the
[DB schema policy](development.md#db-schema-policy-before-v10). Existing version
markers and migration code remain, but schema versions do not advance with
development iterations and older development databases need not be compatible.
The layouts and retained migration behavior below describe the current code,
not a stable upgrade guarantee.

### Controller database (`ctl.db`)

```sql
CREATE TABLE _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE hosts (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE envs  (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE vms   (id TEXT PRIMARY KEY, env_id TEXT NOT NULL, host_id TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX idx_vms_env  ON vms(env_id);
CREATE INDEX idx_vms_host ON vms(host_id);
```

- `_meta.schema_version` = `9`; a database with a **higher** version refuses to
  open with the "upgrade TTstack first" error; `current < 5` runs the backup
  migration (materialize a default `backup` object with a fresh UUID `revision`
  only where the key is absent, preserving existing values and unknown fields).
  Parse each row as a JSON object, not a fully validated VM. An unparsable or
  non-object row aborts the transaction and leaves the version untouched.
- Pragmas: `journal_mode=WAL`, `synchronous=FULL`, `foreign_keys=ON`,
  `busy_timeout=5000`.
- Pending resource updates reserve only growth beyond the previous disk
  reservation. The VM intent and host disk counters commit atomically before
  forwarding; exact retries reuse that charge, and a definitive fresh rejection
  releases it with the intent.
- Row payloads are the JSON documents described in §2 — field names and omission
  rules included. Byte-identical serialization is not required (JSON object order
  is not meaningful), but decoding a Rust-written row and re-encoding it must
  produce the same document as Rust's decode-and-re-encode, including materialized
  defaults. Typed persistence does not promise to retain unknown fields; the raw
  JSON migration and offline converter do. Golden tests assert each contract.

### Agent database (`agent.db`)

```sql
CREATE TABLE _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE vms   (id TEXT PRIMARY KEY, data TEXT NOT NULL);
```

- `_meta.schema_version` = `10`; native schemas 6–9 upgrade as specified in §10–14; other populated versions are
  rejected (no automatic migration from v5 — the offline converter exists for
  that; see [the upgrade procedure](deployment.md#upgrade-and-offline-v5-conversion)). `_meta` also holds `host_id`, `network_config` and
  `container_runtime`.
- Unreadable VM rows are retained, mark the inventory as corrupt, pin resource
  reservations to capacity and disable admission.
- Changing the persisted network configuration or container-runtime binding while
  VM rows exist is refused.

"Populated" means any non-internal schema object exists, not that a VM row exists.
An empty v5 inventory is still rejected. Existing v6 state with missing tables is
not silently repaired. The writer uses a 5000 ms busy timeout and configures
WAL/FULL after the schema gate; read-only snapshots use 2000 ms. The agent does
not enable the controller's `foreign_keys=ON` pragma. Connection-local settings
must be applied to each actual Go connection, not only once to a pool. The offline
converter and host-identity connection retain rusqlite 0.40.2’s default 5000 ms
busy timeout as well.

### Host paths and artifacts

| Path | Content |
|---|---|
| `/home/ttstack/run` (`RUN_DIR`, fixed) | QEMU PID/monitor/seed files, Firecracker PID/socket/config/log/sandbox files, SSH seed scripts, bhyve/jail artifacts |
| `<runtime-dir>/clone-<vm-id>` | per-VM clone (directory or dataset) |
| `<runtime-dir>/.tt-backups/<vm>/<generation>/` | reflink backup: `owner`, `disk`, `ready.json` |
| `<data-dir>/service.lock` | exclusive `flock`, mode 0600 |
| `<data-dir>/backup-<vm-id>.lock` | backup writer lock |
| `/opt/ttstack/etc/api-key` | shared deployment key, mode 0600 |

ZFS names: base snapshot `@ttsnap`; backup snapshots `<disk>@ttbackup-<uuid>` with
user property `ttstack:backup=<vm-id>:<uuid>`; Firecracker zvol layout keeps
`vmlinux` in the parent dataset and the root filesystem in the `rootfs` child
volume, symlinked as `rootfs.ext4` inside the clone.

Backup dependency witnesses hash the exact compact serde JSON of `VmOptions`
with `requested_disk` normalized to zero, followed by each dependency's basename
and file bytes in the recorded order. CPU/RAM changes do not change this input.
As with guest-config digests, Go HTML/Unicode escaping must not change these
bytes. The storage layer takes this serialized options string unchanged; the
agent builds it when preparing a backup context.

## 5. Identifiers that must match bit-exactly

| Identifier | Rule |
|---|---|
| TAP name | `tt-` + zero-padded 12 hex digits of the **low 48 bits** of SipHash-1-3(zeros, `UTF8(vm_id) ‖ 0xff`); ≤15 chars |
| Guest MAC | `02:54:aa:bb:cc:dd`, two lowercase hex digits per IPv4 octet; `10.10.0.2` → `02:54:0a:0a:00:02` |
| Bridge | `tt0`, address `10.10.0.1/16`; with zero-based index `i`, set `n=i+1`, guest IP `10.10.<(n/254)&255>.<n%254+1>`; index 0 → `10.10.0.2` |
| nftables | table `tt-nat` (chains `prerouting`, `postrouting`, `forward`, set `denylist`); isolation tables named from the TAP with `-`→`_`; SSH ingress table `ip tt-ssh` |
| Firecracker jail | UID/GID `100000 + c*256 + d`; chroot `<image-parent>/.jailer/<binary>/<tap>/root`; cgroup `/sys/fs/cgroup/ttstack/<tap>`; socket `/fc-<tap>.sock` (<108 bytes) |
| Docker container | `tt-<vm-id>` |
| systemd units | `tt-ctl.service`, `tt-agent.service`, drop-in `90-ttstack-mounts.conf` |
| CLI config | `~/.ttconfig`, two lines (`addr`, optional key), mode 0600 |

Test vectors for the TAP name come from the Rust tests
(`crates/core/src/net.rs`): `""→tt-6ea523c53def`, `"vm1"→tt-e9e453b4d5a5`,
`"vm2"→tt-fc7c4d3fbec3`, `"83b7e95-test-vm"→tt-ab2cb9caf2f1`,
`"long-id-abcdefghijklmnopqrstuvwxyz"→tt-75e5d7552606`.

## 6. External commands and timeouts

Command lines, file names and timeouts are part of the contract where they affect
other software or the operator (for example the `nft` rule text, `zfs` snapshot
names, `jailer` arguments, `qemu-system-x86_64` arguments and the Firecracker boot
arguments). Defaults to preserve:

| Item | Value |
|---|---|
| Bounded command | 300 s default; stdout/stderr captured up to 4 MiB each |
| Agent/controller background loops | 15 s (reconcile, host refresh, image catalog), 60 s (expiry) |
| Client timeouts | 5 s health, 30 s registration/backup inspection, 360 s mutations, 600 s CLI mutations |
| Creation wait in the CLI | poll every 2 s, up to 600 s |
| QEMU shutdown grace | 10 s (`system_powerdown`), then SIGTERM/SIGKILL |
| Firecracker shutdown grace | 30 s after `SendCtrlAltDel`, then SIGTERM/SIGKILL |
| Backup storage budget | 240 s per attempt, 60 s per command, 16 MiB headroom probe |
| Port pool | 20000–65535 inclusive, at most 256 entries per VM; this downstream adds port 22 automatically only for explicitly requested SSH setup |

QEMU's monitor is HMP text over a Unix socket, not QMP JSON. `cont` resumes a
paused process; agent start of a stopped VM boots the retained disk through the
engine's create path. PID recovery returns absence on a complete zero-match scan,
but errors on unreadable or ambiguous identity. Capture limits apply to returned
command output, not the size of the temporary files Rust uses during execution.

## 7. Deliberate deviations

These are the intentional departures. The downstream defaults listed below
override the pinned defaults; other API and filesystem behavior remains governed
by this contract.

| Area | Rust | Go | Why |
|---|---|---|---|
| Downstream package layout | upstream Go module `github.com/TTstack/tt` keeps API/model packages internal | module `github.com/aisidev/tt` publishes `api` and `model` | retain the downstream's existing secondary-development interface |
| Default environment lifetime | six hours | zero, meaning permanent unless an explicit lifetime is supplied | retain the downstream's existing persistent-environment default |
| Automatic SSH forwarding | QEMU, bhyve and Jail add port 22 implicitly | automatic injection requires explicit SSH setup; explicitly requested ports remain honored | retain the downstream's existing opt-in ingress behavior |
| Optional VM stop grace | fixed per-engine defaults | defaults preserved; `--stop-grace` accepts -1..300 seconds for Linux VMs | explicitly authorized operator control; zero requests immediate force |
| Downloads (image recipes, deployment helpers) | `curl` | Go HTTP client | fewer host prerequisites; same URLs, digests, atomically published files |
| Firecracker API calls | `curl --unix-socket` | HTTP client over the Unix socket | same requests, same 5 s timeout |
| CLI argument parsing | clap | explicit subcommand tree | dependency policy; pinned help and output fixtures, equivalent validation and exit codes |
| Error diagnostics | `ruc` may decorate business errors with ANSI escapes, timestamps, PID/namespace and Rust source locations | Go contextual error text without that process-specific decoration | preserve stable business messages and status/prefix mapping; avoid fabricated Rust stack locations |
| Async runtime | Tokio | goroutines/context | idiomatic Go |
| SQLite driver | rusqlite (cgo, bundled) | modernc.org/sqlite (pure Go) | no cgo; behavior fixed by pragmas and fixture tests |
| HTTP services | axum/hyper reads request headers without a timeout | 10 s `ReadHeaderTimeout` on both servers | bound drip-fed connections (Slowloris); complete requests at any speed are unaffected, no timeout is added for bodies or responses |
| Unknown JSON member depth | serde_json skips ignored members iteratively, at any nesting depth | `encoding/json`'s 10 000-level cap rejects bodies whose *unknown* members nest deeper | a faithful iterative skipper was judged disproportionate; only pathological bodies carrying >10 000-deep unknown members differ, and declared fields are unaffected |
| Docker creation after a lost response | a retry conflicts forever on the leftover container | a follow-up `inspect` adopts the container iff its image matches the immutable spec | a timeout must not prove creation failed; retries heal instead of wedging |
| Host reservation release | reservation drift converges on the 15 s refresh | a confirmed deletion returns the VM's reservations in the removal transaction | immediate availability; the accounting ceiling and the conservative floor are unchanged |
| Undecodable controller rows | every recovery read drops them (phantom rows keep their last state) | replaced with offline/failed markers carrying an operator-facing error; an offline host marker with no address can be re-registered by its verified host ID, retaining tracked VMs and reservation floors | keep damaged records visible and allow recovery without detaching their VMs |
| Guest-config admission | 32-file/64 KiB limit judged without the managed SSH seed at admission and with it at runtime | the seed-inclusive payload is judged at admission, matching the documented limit text | one Firecracker + SSH shape now fails earlier with a clear 400 instead of a runtime VM failure |
| Deploy diagnostics for missing binaries | `run 'cargo build --release'` | `run 'make release'` in local and distributed deployment, checked per deployed role | Go build instructions; agents-only deployments need no controller binaries |
| FreeBSD experimental label | bhyve and Jail combinations set `experimental` to true; CLI, help, dashboard and the agent startup warning call FreeBSD experimental | the JSON field remains, and Go emits `false`; platform stays `freebsd`; human text says validated only on FreeBSD 15.x; the startup warning is not printed | native Jail, bhyve file and bhyve zvol ran on FreeBSD 15.1; other FreeBSD releases are not claimed |
| Custom controller deployment directory | Source-dependent behavior unverified without the pinned checkout | Prepare and assign the configured `controller.data_dir` before starting its service | Native Open Math deployment reproduced permission denial with the inherited home-only setup; default template fixtures remain unchanged |
| Remote activation check failure | Pinned template permits a failed `is-active`, OpenRC status or manual process check to be masked by later cleanup | Each check is a standalone command under `set -e`; failure stops success reporting | A failed service is not a completed deployment; SSH/CLI errors retain possible partial effects |

Native phase-4 HTTP observation established that `ruc::Error::to_string()` can
include a timestamp, process ID, ANSI styling and source location inside the
JSON `error` string. These bytes vary across executions and build layouts. The
Go port retains the stable business message and contextual causes, but omits
that Rust diagnostic envelope. Differential tests explicitly normalize only this
envelope when comparing business failures; they do not normalize protocol
prefixes, status codes, JSON extractor errors or routing responses. This corrects
the earlier plan's overly broad promise of identical error strings.

CLI debug and diagnostic formatting may differ from Rust, as authorized for this
port; operation behavior, exit status, stable business prefixes and protocol
fields remain contractual. Deployment diagnostics redact `TT_API_KEY=` lines.
Go-native transport/storage/TOML diagnostic causes may also differ, including
human-readable `host.error`, `env.error` and `vm.error` text. Stable business
prefixes and status mapping remain exact. Header names are case-insensitive;
canonical spellings in this document do not prescribe wire capitalization.
CLI JSON maps with unsigned port keys retain numeric BTreeMap order. API clients
disable redirects and automatic retries; Go uses a fresh HTTP/1 connection for
each request to prevent transport replay of backup requests carrying idempotency
headers. Image downloads use a 15 s connect and 900 s overall timeout, an 8 GiB
limit, the curl limit of 50 redirects and the same pinned or manifest digests.

Anything else that changes a JSON field, a status code, a printed line, a path or
a host-visible name is a compatibility bug, not a design choice.

## 8. How compatibility is verified

1. **Golden JSON fixtures** per wire type, generated from the pinned Rust build
   with positive and negative decode cases; semantic round trips preserve number
   precision, presence and null rules. Byte fixtures cover digest input and text.
2. **Database fixtures**: checked-in synthetic SQL/JSON exports reconstruct
   temporary databases in Go tests. Differential tests create actual databases
   through Rust, mutate them through Go and reopen them through Rust. Record the
   oracle revision; never commit database files or private inventory.
3. **Identifier vectors**: TAP names, MAC addresses, IP allocation, digest and
   command-line golden tests.
4. **Cross-implementation runs** (phase 7, authorized hosts): Rust `tt-ctl` with a
   Go `tt-agent`, and Go `tt-ctl` with a Rust `tt-agent`, plus a Go CLI against
   both, on the same schema versions.

## 9. Reviewed input, process and shutdown boundaries

- `--` ends option/subcommand recognition. It can precede a leaf command's
  positional value but cannot introduce another command. A lone `-` is a string
  option value. Service resource/port flags use decimal unsigned integers,
  including a leading `+`; Go's octal/hexadecimal flag syntax is not accepted.
- Missing requirements are reported together in deterministic order. Parent help
  and help-subcommand help are available; error wording/order need not reproduce
  clap's cosmetic suggestions (see §7).
- Non-UTF-8 PID/process metadata is an unknown identity, not evidence of exit.
  Valid-text missing/stale PID metadata may still recover the exact process by
  marker. The prepared Firecracker version marker uses Rust's ASCII whitespace
  set, which excludes vertical tab.
- Controller shutdown first drains foreground HTTP handlers, then cancels detached
  creation/expiry waits while preserving durable plans and reservations. It waits
  for its own workers to exit before releasing the database/state lock. The agent's
  accepted-operation draining policy is unchanged. Client disconnection is distinct
  from service shutdown; accepted work may continue after disconnection.
- CLI runtime detection/pull, image inspection, `freebsd-version`, local deployment
  helpers and `scp` retain the Rust call sites' absence of an elapsed-time limit.
  SSH scripts remain bounded at 900 s; recipe conversion/population commands and
  core host operations keep their existing explicit bounds. Uncapped CLI commands
  still honor cancellation and the documented capture limit.
- Firewall stdin and reflink qualification use open, unlinked scratch files.
  Database compatibility means schema, pragmas, typed JSON values and recovery
  semantics; it does not promise identical SQLite page files, DDL whitespace or
  generic JSON key order/number spelling. Explicit digest inputs remain byte-exact.

The FreeBSD memory-discovery command remains bounded at 300 s and uses the
existing 8192 MiB fallback on failure; no guarantee of an indefinitely hung
startup probe is made.

The independent review and evidence are recorded in
[audit-resolution-2026-09-30.md](validation/audit-resolution-2026-09-30.md).

## 10. Durable command envelope (optional extension)

The optional protocols in §10–15 extend the pinned release under explicit
operator authorization. Legacy HTTP requests remain supported. This downstream
retains its permanent default lifetime and opt-in automatic SSH forwarding as
specified in §7. Database handling follows
the pre-v1.0 development policy in §4. Versioning and existing migration code are
retained; further development changes update the latest schema in place without
requiring compatibility with previous development databases.

`tt-agent --stop-grace` sets the default grace for Linux QEMU/Firecracker:
`-1` preserves 10/30 seconds, `0` skips the graceful attempt, and a positive
integer up to 300 supplies seconds. Values outside -1..300 are rejected before
engine construction, preventing duration overflow from becoming immediate force. Per-call policy in §11 overrides that default for
one operation. Other engines retain their existing shutdown behavior.

Environment lifecycle clients may send `X-Compute-Command`, a JSON object with
`subject`, positive `generation`, positive signed-64-bit `token`, `key` (1–200
characters) and a 64-character SHA-256 `fingerprint`. The subject is the
environment ID and must end in `-g<generation>`. Controller-to-agent calls
forward this identity, suffixing the step key with the inner VM ID. This header
contains identities and digests, never bearer credentials or guest secrets.

Both receivers commit the highest token and a request receipt before dispatch.
A lower token fails with `state/stale_fence`; the same key with a changed
fingerprint, method, path or body fails with `terminal/fingerprint_conflict`.
Successful acceptance is replayed from the receipt. Failed/pending attempts can
be retried with the same identity. Watermarks and receipts survive deletion and
restart. A subject that has accepted a fenced command rejects subsequent legacy
unfenced mutations. Unfenced subjects retain the legacy API behavior.

Classified failures use `{ok:false,failure:{class,code,message}}`. Classes are
`denied`, `terminal`, `state`, `transient`, `rate_limited` and `unknown`.
`Resp.failure` is optional and omitted by legacy responses. Receiver refusals
retain their class through controller forwarding; messages never drive retry
classification. Existing unclassified legacy responses keep their old contract.

Controller `EnvDetail` adds optional `observation_revision`. A successful complete
snapshot allocates a signed-64-bit sequence from a persistent controller clock
while holding the snapshot lock. It advances across repeated lifecycle cycles
and controller restarts; zero means the sender does not implement this extension.
It is a sample ordering guarantee, not proof of guest application readiness.

Controller schema 6 adds `observation_clock`, `command_fences` and
`command_receipts`. Agent schema 7 adds the latter two tables; a structurally valid
native schema 6 upgrades transactionally to 7 before admission. Other populated
agent versions remain refused. The v5 offline converter still emits native v6,
which upgrades at the next agent start. Incomplete databases are never repaired
by this migration. These extensions intentionally diverge from the pinned Rust
wire oracle; legacy fixture shapes remain unchanged when fields are absent.

## 11. Per-call stop policy (optional extension)

`POST /api/envs/{id}/stop` and `POST /api/vms/{id}/stop` accept an optional JSON
object with `grace_seconds` (1–300), `force` (boolean) and `graceful_only`
(boolean). An absent body or `{}` preserves engine/agent defaults. `force`
skips graceful shutdown; `force` and `graceful_only` are mutually exclusive.
A zero explicit grace, an out-of-range grace or conflicting flags is
`terminal/invalid_stop_policy`, before any engine effect.

Linux QEMU and Firecracker implement this policy. Other engines refuse a
non-default policy with `terminal/unimplemented`. Policy is copied into one engine
instance and never changes the fleet-wide default. Graceful-only expiry returns
`state/graceful_timeout`, retains the process and disk, and never invokes the
force-kill path. A later explicit force is a separate command identity. Custom
grace without `graceful_only` follows the normal graceful-then-force progression.
The controller forwards these fields and preserves typed agent refusals.

`tools/native/stop-policy.py` exercises the three modes using a boot-only
Firecracker image without a shutdown handler. It requires an explicitly named
controller and key file and cleans only the unique environment it created.

## 12. Persistent disks and immutable volume checkpoints (optional extension)

The persistent disk API is negotiated independently of build version.
`GET /api/storage-capabilities` returns `StorageFeatures` with
`protocol_version: 1`, `volumes`, `snapshots` and `data_disks`. Each agent's
`AgentInfo.storage_features` and the controller's `Host.storage_features` report
that agent's support. A missing report or an unknown protocol is unsupported;
modern placement never sends disk fields to an older agent that would ignore them.
Linux file and zvol backends have native qualification; other hosts advertise
these features as unavailable until qualified.

Both services expose:

| Method | Path | Meaning |
|---|---|---|
| POST | `/api/volumes` | create a blank raw disk, image root or checkpoint clone |
| GET | `/api/volumes` | bounded keyset page, `size` 1–100, `after`, optional `owner` |
| GET | `/api/volumes/{id}` | read authoritative disk metadata, including tombstones |
| PATCH | `/api/volumes/{id}` | grow capacity and/or change explicit retention |
| DELETE | `/api/volumes/{id}` | explicit offline destruction after dependencies clear |
| POST | `/api/volumes/{id}/attach` | offline binding to a co-located VM |
| POST | `/api/volumes/{id}/detach` | offline detachment |
| POST | `/api/volumes/{id}/snapshots` | create a unique immutable checkpoint |
| GET | `/api/volume-snapshots` | bounded checkpoint page |
| GET, DELETE | `/api/volume-snapshots/{id}` | inspect or explicitly remove a checkpoint |

Every mutation requires the §10 command envelope. Volume identities end in their
own `-g<generation>`; checkpoint mutations carry the checkpoint subject. A
checkpoint create is fenced on its source volume and takes `{id}` as its body.
Binding takes `{vm_id, role}`; the VM ID is the inner VM identity and role is
`root` or `data`. Disk observations include the current attached environment
identity as well as the inner VM ID. Callers never submit a host filesystem path.

`VolumeSpec` supplies `id`, `generation`, `size_mib` (1–1048576), explicit `retain`,
optional `owner`, `host_id`, image+engine for an initialized root, or
`from_snapshot` for a new clone. Blank data disks are raw. Root images support
QEMU/Firecracker and use the backend's existing root format. A source checkpoint
fixes locality, image and engine; restoration never changes the source and never
shrinks it. Volume and checkpoint metadata persist after deletion as tombstones.
IDs are not reused, including after deletion. Lower command watermarks
and changed request fingerprints are rejected before physical effects.

Persistent VM creation adds named object fields `durable_root`, `root_volume_id`,
`root_retain`, `data_volume_ids`, `host_id` and `initial_stopped`. An automatic root
is identified deterministically from the environment and VM slot and defaults to
retained. Explicit roots and all data disks must already exist, be available and
share one host. The limit is eight data disks per VM. QEMU and Firecracker attach
those raw disks at launch. Root replacement requires explicit offline detach and
attach. Stopped provisioning spends disk capacity without consuming running seats.
It does not boot the guest; a subsequent start is a distinct command.

A managed VM's deletion stops and destroys the engine process, fences/detaches
its disks, and preserves those independent disks. It never implicitly destroys
managed data, even when a disk's retention flag is false. The caller's journaled
GC phase performs explicit volume destruction. Destruction refuses retained or
attached disks and any live checkpoint dependency; retention must first be
cleared explicitly. Legacy VMs without persistent-disk fields keep their existing
clone/deletion semantics.

A disk becomes available only after its journaled initialization and durability
barrier. Retrying a creation may discard an owned *unpublished* stage; an already
available disk is never silently recreated, even if its bytes later disappear.
Growth is offline and grow-only, with the larger reservation committed before
I/O. A pending checkpoint fences starts, attachments and growth until the
checkpoint settles. File checkpoints are independent private copies; zvol
checkpoints retain ZFS ancestry. Every restored Firecracker dataset rewrites its
root-device symlink to its own cloned child, preventing source-device aliasing.
Restored disks retain their checkpoint dependency until explicit destruction.

Controller placement atomically records the VM plan and root-volume reservation;
replay retains the original VM ID, disk identity and host. Modern environment
creation runs to an observed engine/stopped result before returning acceptance;
a partial result remains outcome-unknown and resumes the persisted plan. The
application must still establish guest runtime readiness separately. Requests
are bound by a controller-computed creation hash. Root metadata/kernel overhead
is included in host disk admission; independent disks are charged once and remain
charged after VM deletion. Unknown initialization, growth and checkpoint outcomes
keep their claims. Late observations cannot erase growth reservations or revive
deletion tombstones.

Controller schema 7 adds `volumes` and `volume_snapshots`; agent schema 8 adds
those catalogs after the command-journal migration. Each upgrade is transactional
and preserves existing VM rows. Named optional JSON fields carry the new data;
the legacy positional tuple maximum remains pinned, so an old extra tuple member
cannot acquire a new meaning. The norm for existing paths remains unchanged;
persistent disks use `<runtime-dir>/volume-<id>` and file checkpoints use
`<runtime-dir>/checkpoint-<id>`. These are host-local paths only.

`tools/native/volumes.py` validates real root/data persistence, offline rebinding,
start/stop, growth, checkpoint cloning, dependency guards and cleanup. It accepts
an explicitly isolated agent or controller and separate file/zvol fixture names.
The application-level quota, sealed-backup and sweep protocols are separate gates;
this API alone does not claim their completion.

## 13. Complete workspace recovery manifests (optional extension)

`StorageFeatures.workspace_snapshots` negotiates complete stopped-workspace
checkpoints. `POST /api/envs/{id}/recovery-points` accepts `{id}` under the source
environment's fenced identity. `GET /api/recovery-points` is a keyset page (1–100,
`after`, optional `environment`), and `GET`/`DELETE /api/recovery-points/{id}`
inspect or destroy a manifest. Checkpoint IDs carry their own source generation.

A manifest captures exactly one stopped managed VM, its profile, and the complete
root/data disk set. The agent first persists a `checkpoint_freezes` record and a
writer fence on every participating disk. Starts, resizing, deletion, rebinding
and independent snapshots fail while that fence is held. The controller captures
the agent's exact frozen profile, allocates immutable child checkpoint identities,
creates every child, then thaws the same freeze identity. A manifest publishes
`ready` only after all children and thaw are acknowledged. Freeze, each disk
snapshot and thaw use distinct deterministic step keys. A crash resumes the
recorded disk set and sequence, never a newly guessed set of files.

`POST /api/envs/{new-id}/restore` takes `{point_id,owner,cpu?,mem?,guest_config?}`
and requires a new environment identity. The controller journals an immutable
restore plan, clones every source disk into a new disk identity in the source
locality, and provisions the new VM **stopped**. The source manifest/disks are
unchanged. A subsequent explicit start has its own identity and admission.
Restored disk dependencies prevent destruction of their checkpoint ancestors.

Deleting a creating point is an explicit abort. It advances source environment
and volume fencing watermarks, removes only that point's partial children, and
thaws only its own freeze identity. A failed abort remains visible and retained;
it is not timed into success. A ready-point deletion is mark-then-sweep and may
remain `deleting` while a restored disk still depends on a child checkpoint.

Controller schema 8 adds `recovery_points` and `recovery_restores`. Agent schema 9
adds the durable `checkpoint_freezes` table; older agents reject that format.
The new pointer-free wire models are named objects, separate from legacy tuples.
`tools/native/recovery.py` validates the complete manifest, cloned root/data
contents, new stopped VM, source preservation, boot, ancestry refusal and cleanup.

## 14. Portable disk archives and image cache (optional extension)

`StorageFeatures.archives` and `.image_pull` negotiate this protocol separately
from build versions. `GET /api/volumes/{id}/archive` exports an available,
unattached settled disk through the authenticated controller. Export holds
subject exclusion against writes, but streams outside the global lifecycle lock.
The agent has two independent transfer slots. Exports and imports time out after
30 minutes; neither a disconnect nor expiry proves resource absence.

The portable format is one gzip stream containing a USTAR archive, with a regular
`manifest.json` first (maximum 16 KiB), followed by exactly its ordered regular
members and the standard two-block tar terminator. No links, directories, PAX
extensions, undeclared entries or trailing bytes are accepted. The manifest is
`DiskArchive{version:1,engine,image,format,size_mib,members:[{name,bytes}]}`.
Firecracker uses `vmlinux` (nonempty, at most 64 MiB), `rootfs.ext4` (exact disk
length), and optional `guest-config.ext4` (exact configured config-disk length).
QEMU uses a single `disk.raw` (exact disk length) or `disk.qcow2` (at most disk
size plus size/128 plus 8 MiB). Blank data disks use exact-length `disk.raw`.
QEMU validation additionally checks the admitted virtual size, rejects backing
files, external data files and encryption, and runs the image integrity checker.
File backends publish QEMU disks as qcow2; zvol backends publish raw devices.

`POST /api/volumes/{id}/import` accepts
`{pull:{id,sha256,bytes,expanded_mib},spec:VolumeSpec}`. The compressed SHA-256,
exact compressed length and expanded member budget bind the immutable intent.
Root engine/image/size must match its manifest. Admission reserves both the new
disk and private staging bytes before downloading. The URL is transported only
in `X-Archive-Source`, outside the semantic command fingerprint, so presigned
access can be refreshed without changing intent. Executors accept only exact
configured origins in `TT_ARCHIVE_SOURCES` (comma-separated host:port authorities),
with no redirects, userinfo or fragments. Origins are operator trust boundaries;
configure only private archive brokers or intended object-store endpoints.

Agent schema 10 adds `archive_imports`. A journal owns each private stage under
`<data-dir>/archive-stages/<id>` and its new unpublished target. Verification and
sync precede the durable ready marker. File publication uses exclusive copies
when the state staging directory and runtime/image storage are on different
filesystems; an existing target is never adopted. A crash retries only that unpublished
stage, never an available disk. Post-admission failures retain the journal and
claims and report `unknown/archive_incomplete`. Explicit disk destruction also
cleans its stage and tombstones the import; ordinary creation cannot adopt an
unfinished import. Transfer identities and fencing use §10.

`POST /api/images/{sha256-<digest>-g1}/ensure` asks the controller to pull the
same admitted archive into qualified online hosts, through agent
`POST /api/images/{id}/pull`. Image IDs bind the compressed archive digest.
Image staging and cache bytes are charged to host capacity. Catalogs and VM
creation require the durable image-ready marker, so a visible partial directory
cannot become a bootable image. The controller preserves unknown partial fleet
outcomes for replay. The cache is immutable; its physical directory alone never
constitutes admission. Sealing and object-store credentials belong to callers,
not this generic executor protocol.

Profile-bound image administration additionally requires
`StorageFeatures.image_catalog` from controller and agent. Ensure accepts an
optional `spec:{engine,size_mib}`; a nonempty expected engine requires the archive
manifest's exact engine and root size before publication. Legacy callers that
omit this profile retain the original archive contract. Unknown feature support
is rejected by profile-bound callers.

Controller schema 9 adds `image_catalog(id,data)`. A row binds the full immutable
pull and optional profile to a complete bounded set (maximum 4096) of receiver
`{id,addr}` pairs before any dispatch. Retry preserves that set through host
catalog changes. Offline, removed or replaced receivers remain unknown. A fresh image ensure command key may extend the durable set with newly
qualified receivers; replay keeps its last accepted set. Extension never removes
an earlier receiver, including one currently offline.
`POST /api/images/{id}/fence` propagates a higher subject fence to every recorded
receiver without deleting caches or modifying their data. `DELETE /api/images/{id}/cache` on the
controller marks its plan retiring and dispatches the higher image command to
that same set; the controller refuses later ensure on a retiring plan. Agent
cache deletion checks both VM and retained-disk references, drains earlier image
commands through subject exclusion and cleans journal-owned cache/stage bytes.
An unadmitted or already deleted cache needs a complete bounded namespace and
stage absence proof; unowned bytes are quarantined and never deleted by name.
An absent controller plan proves only that this controller never dispatched a
pull; physical caches after catalog rollback require the epoch recovery protocol.

`tools/native/archive.py` validates root/data round trips, refreshed-grant replay,
restored boot, content-addressed image pull/boot, and corrupted-byte refusal on
explicitly isolated file/zvol agents. Its HTTP source must use a configured
private origin. Run the harness inside the agent's mount namespace for zvol
root-device access, as with §12–13.

Agent `DELETE /api/images/{id}/cache` explicitly sweeps only a journal-owned
content cache under its fenced image identity. It refuses live VM/disk
references, marks the cache unavailable before physical cleanup, retains claims
on failure, and refreshes the catalog only after durable deletion. This cold
lane does not hold the lifecycle mutex while destroying storage. A later pull
can re-materialize the exact same immutable digest with a fresh command key;
watermarks and receipts survive eviction. User disk identities remain tombstoned
and cannot be reused. Native archive harness cleanup uses this private agent
endpoint after deleting its own VM/disk dependencies.

Root-disk placement also requires the requested engine, its qualified disk-size
capability, an advertised base image with a known sufficient size, and capacity.
Portable imports carry their own verified base bytes and therefore require engine
qualification without requiring that source image to be pre-cached. A partial
controller plan is cleaned only through the executor's fenced delete ACK. If an
executor never admitted a disk row, it checks VM bindings and physical absence;
unowned bytes, dangling paths and failed probes remain unknown and untouched.
Only a successful absence ACK permits the controller's corresponding tombstone.

`StorageFeatures.archive_abort` separately negotiates
`POST /api/volumes/{id}/import/abort`. Under a higher fenced command, it discards
only an unsettled import whose durable journal proves it was never ready or
attached. Tentative retention belongs to that unpublished stage and cannot block
its explicit abort. A ready import refuses this action and remains governed by
normal disk retention. Missing ownership, live VM references, unowned bytes and
failed probes remain unknown. The controller requires the feature report before
forwarding and records absence only after a positive executor acknowledgement.

## 15. External control epochs and physical recovery evidence (optional extension)

`--authority-file` configures a private absolute JSON file outside the SQLite
restore scope. Its version-1 fields are `version`, `authority` (canonical UUID),
`epoch` (1..8388607), `phase` (`fenced`, `inventory`, `reads`, `mutations`, `gc`),
and `key` (canonical decision UUID). Receivers require bearer authentication and
an explicitly stable host ID. Missing/malformed/unavailable files fail closed;
an observed authority-ID change or epoch decrease is refused.

Modern command envelopes add `authority` and `epoch`. The fencing token composes
`epoch << 40 | sequence`, with sequence in 1..1099511627775 and the result fitting
a signed 64-bit counter. Receiver-wide epoch validation precedes receipt lookup,
including a success receipt restored from old SQLite. Unknown subjects get the
same global check. Normal commands are blocked before the mutation phase;
unregistered mutations fail closed. Legacy expiry/deletion and agent automatic
reconciliation do not run in this modern profile. Read-only host catalog refresh
waits for its verified serving gate.
The legacy profile remains explicitly unbound to this extension.

Serving phase hints alone do not grant command authority. `activation_version: 2`
in the control report negotiates `POST /api/control/resume`. Its body names
`phase` (reads, mutations, GC), `fence_id`, `previous`, `inventory_digest` (host
only), `catalog_digest` and `plan_digest`. The receiver verifies its own current
fence and a strict phase chain. A host recomputes complete frozen inventory and
requires zero owned payload processes before reads and mutations. GC drains
earlier receiver commands and storage transfers; its receipt attests a command
barrier and the prior phase chain, not shutdown of legitimate current-epoch VMs.
Controllers attest only their own gate and command barrier. The authenticated
caller's catalog/plan digests bind its reviewed composition; they cannot replace
the receiver's physical evidence.

External `<authority-file>.activation-reads`, `-mutations` and `-gc` documents
contain version, UUID, authority/epoch, epoch decision key, receiver, command key,
method, scope digest, exact request and completion timestamp. Receipts publish
before the phase file. A retry can finish an interrupted phase write using the
same immutable receipt. Complete current-epoch history is returned in `activations`.
Mutation admission requires the entire matching external phase chain as well as
the epoch file; SQLite loss or a serving-mode hint cannot manufacture it. DELETE
and unpublished-import abort require the separate GC gate.

Version-2 activation receipts also persist `quarantine_mib` and
`quarantine_pinned`, computed by the receiver from its own frozen inventory.
Every phase in the chain must preserve those claims. They are restored before
serving after a process restart, independently of SQLite rows. Controllers report
zero quarantine capacity; they cannot replace host accounting. Upgrading a bound
receiver from the earlier development receipt format requires a new fenced epoch.
A missing receipt or incompatible chain blocks admission.

Inventory items report `capacity_mib`, `claim_mib` and `capacity_unknown`. The
complete report sums capacity exceeding durable metadata claims into
`quarantine_mib`; an unknown bound sets `quarantine_pinned`. Both totals are checked
against complete item pages. Admission and `/api/info` include this independent
claim; an unknown bound consumes the host's entire disk budget. A timeout, missing
SQLite row or later file disappearance cannot release that external claim. A new
reviewed epoch may establish a lower bound after explicit resource resolution.

Sparse files are measured by logical length. Bounded qcow2 v2/v3 header inspection
uses virtual capacity plus the existing root metadata envelope, without opening
backing or external data paths; it establishes no image validity or ownership.
Unsupported layouts remain unknown. Fixed Firecracker members are measured as
raw files. ZFS uses referenced/logical-referenced and volume size separately,
avoiding parent/subtree `used` double-counting. A Firecracker filesystem alone
cannot hide a missing `rootfs` child. Image pulls must fit their admitted virtual
capacity as well as their bounded transfer envelope. Unpublished stage traversal
is bounded and never follows links.

The modern agent does not replay legacy automatic restart/resize/backup recovery
loops when gates open. The modern controller disables legacy expiry/deletion
jobs. Explicit current-epoch commands drive effects; host catalog refresh remains
an observation and cannot replace unreadable ownership rows with placeholders.
The unbound legacy profile retains its original automatic behavior.
Modern `/api/info`, VM list and VM get project bounded read-only QEMU/Firecracker
process observations. Two independent request lanes and four probes per request
bound work; a changed database/authority snapshot or failed identity probe returns
a typed failure. Runtime/journal/network/disk rows are not repaired or settled by
these reads. Pending creation/deletion/resize/backup intent keeps its recorded
state. Advertised computation retains at least the durable claim even when a
process has exited. A live process contradicting a recorded stopped state pins
admission for explicit recovery. A verified physical fence clears that fault.

Controllers advertise `enrollment_version: 1` for `POST /api/control/enroll`.
This is a normal current-epoch mutation under the control subject, after serving
activation. The request pins `host_id`, private HTTP `endpoint`, `scope_digest`
and the exact activated `plan_digest`. The controller verifies the authenticated
agent's current authority, epoch, scope and mutation receipt before registering
that exact peer. The returned command receipt binds the request; same-key replay
is stable. This action records membership only: agent VMs/disks absent from
controller ownership are never adopted. Legacy `/api/hosts` behavior remains
available only in the explicitly unbound profile.

`GET /api/control` returns protocol version, enabled authority/epoch/phase and a
current-epoch fencing receipt when present. Enabled process-backed agents also
report `scope_digest` and `engines` before fencing, so an external deployment plan
can pin the exact process/storage namespace separately from restored routing rows.
`POST /api/control/fence` requires
subject `control-<authority>-g<epoch>`, generation equal to epoch, current epoch
and a stable command key. A host drains earlier admitted commands, pins owned
processes with pidfd, verifies exit and persists stopped observations before
committing an external receipt and advancing only to inventory. A controller
returns the distinct `controller_commands_drained` method, which proves no host
shutdown. Physical node fencing may not be inferred from credential refusal,
link loss, an empty restored VM table or the controller's receipt.

The process-backed epoch profile supports QEMU and Firecracker and advertises
that exact scope. Other workload engines require their own physical verifier.
Each such receiver has an exclusive kernel payload domain, named from the full
SHA-256 of immutable authority/receiver identity. Firecracker's jailer places
children below that cgroup; QEMU uses `CLONE_INTO_CGROUP` through an opened cgroup
FD. This domain remains outside SQLite state. A guest whose namespace exposes
root and executable paths as `/` and `/firecracker` remains attributable by its
kernel cgroup membership. Storage writers are also checked against the receiver's
opened namespace roots. Identity-read failures retain uncertainty. No process
outside the receiver scope is signaled.
The scope digest includes the kernel boot identity. Serving startup rechecks the
current namespace/boot scope against its external fence; configuration movement
or a different kernel requires a new fenced recovery decision. This identifies
the attested kernel domain and does not certify unrelated legacy workloads or
the shutdown of an unreachable physical machine.

The bounded external receipt at `<authority-file>.fence` contains version, UUID,
authority/epoch, receiver, operation key, method, supported engines, scope/process
identity digests, known VM count and completion timestamp. It survives SQLite
rollback and same-key replay preserves the receipt. An epoch change invalidates
it. Only the node's payload scope is attested; actual platform read/mutation/GC
resume still requires complete inventory and explicit ownership resolution.

`GET /api/control/inventory` captures physical namespace and catalog evidence on
one independent lane. `size` is 1..100. Further pages require `snapshot` plus its
`after` cursor; the complete captured identity/digest remains fixed for one
minute. Namespace enumeration is bounded to 65536 entries, 65536 total physical
parts and 16 parts per logical resource. Failed or changing inventories return a
typed error, never partial evidence of absence. Volume/checkpoint payload probes,
base/cache images, private archive stages, runtime markers, jail layouts and
scoped process identity digests are included. Symlinks are observed without
traversal. `frozen` describes command admission, not proof of physical shutdown.
Items identify metadata-only, physical-only and mismatched payload facts; they do
not authorize automatic adoption, deletion or reservation release.
Runtime items carry `subject` as the environment identity independently of the
physical VM UUID. Volumes report their attached environment in `subject` and
source snapshot in `parent`; snapshot `parent` identifies the source volume.
These references are metadata observations, never adoption authority.
Runtime items additionally report generation, engine/image, CPU/memory and exact
root/data disk IDs. `unsettled` marks pending legacy resources, backup artifacts
or incomplete managed creation. Volume items mark pending snapshot/group/grow
intent. These fields let an operator validate a complete stopped group without
turning stale controller liveness into a new observation.

`tools/native/epoch.py` runs on an explicitly disposable receiver unit. It keeps
a real guest across receiver shutdown, restores SQLite from before guest creation,
bumps the external epoch, proves stale-command refusal separately from physical
fencing, then confirms pidfd termination, orphan inventory and preserved data.
Original services are checked by explicit PID-preservation assertions.

## Distributed controller state-directory preparation

The Open Math downstream deployment adapter prepares the exact configured
`controller.data_dir` before starting its unprivileged controller. A nondefault
directory is created and its directory entry is assigned to the configured
service user/group; existing database contents are never recursively rewritten,
removed or replaced. Default-directory template output remains unchanged.

This is an explicit correction to the Go distributed deployment adapter. The
committed pinned default-template fixtures remain unchanged. The reference Rust
checkout is unavailable here, so source-dependent custom-directory parity is
unverified. The original native deployment passed the custom path to `--data-dir`
but prepared only `$HOME/ctl`; a controller under a new `/var/lib/...` path failed
with permission denied. The Open Math isolated systemd fixture retains that
pre-fix observation. This correction changes directory preparation only, not
wire/state formats or the meaning of installer completion and provider readiness.

## Remote activation failure propagation

A remote installation does not print its completion marker after a failed
systemd active check, OpenRC status check or manual process check. These checks
are standalone commands under the inherited `set -e`, rather than the left side
of `&&`, where shell error handling permits a later cleanup command to mask the
failure. Successful output and exit behavior are unchanged. A failed check now
returns a nonzero SSH/CLI result; partial installed files/services are retained
for inspection and a failure does not prove rollback or absence of effects.

The original pinned Rust fixture is retained. Its consumer applies only the
three explicit status-command deltas documented in §7 when comparing the modern
Go remote template. No arbitrary diagnostic normalization or fixture rewrite is
introduced. This corrects reporting only; process-active evidence still does not
establish application or provider readiness.
