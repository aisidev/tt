# Deployment

Build the Go binaries with `make release`. The CLI retains the Rust default
release directory (`./target/release`) for compatibility; pass `--release-dir
./bin` for a Go build. Deployment installs binaries and services. It does not
install hypervisors, prepare images, provision ZFS or register agents.

## Prerequisites

Build with Go 1.26 or newer. `make release` produces CGo-disabled executables;
there is no C compiler or libc requirement for the Go control-plane binaries.
Native engine tools retain their own operating-system and library requirements.
Linux agents need root; the controller normally runs as `ttstack`.

| Workflow | Required host preparation |
|---|---|
| QEMU/KVM | Usable `/dev/kvm`, `qemu-system-x86_64`, `qemu-img`, `genisoimage` or `mkisofs` |
| Linux VM networking | Full `iproute2`, `nftables` and TUN/TAP support; agent startup re-applies the NAT template, which is idempotent on nftables 1.1.x (verified on 1.1.6). On older daemons a replay after restart may fail until the managed table is recreated: `nft delete table ip tt-nat` (guests keep their disks; forwarding resumes when the agent replays) |
| Firecracker | Compatible `firecracker`/`jailer`, KVM, writable cgroup v2 CPU/memory/PID controllers, prepared kernel/rootfs |
| Config drives / ext4 growth | `mkfs.ext4`, `debugfs`, `e2fsck`, `resize2fs` and associated e2fsprogs tools |
| Managed SSH | Host `ssh-keygen`/`ssh-keyscan` and the [guest prerequisites](ssh.md) |
| Containers | Working Docker or Podman runtime and a long-running image command |
| Zvol / file backup | Prepared ZFS datasets, or a qualified Linux reflink filesystem; see [storage](guest-images.md#storage-and-recovery) |
| Distributed deploy | Local `ssh`/`scp`, target `sudo`, and prebuilt binaries for the target OS/architecture |

Go handles recipe HTTP downloads directly; `curl` is not a Go recipe or
Firecracker API prerequisite. Firecracker root preparation additionally needs
`dd`, `tar` and mount/unmount privileges. FreeBSD uses native `fetch`/`tar` and
manual bhyve/Jail/PF preparation.

Reserve jailer UID/GIDs 100000–165535 from unrelated host accounts. Keep runtime
storage private and paths short enough for Unix sockets. Each Firecracker VMM
has a vCPU-count CPU quota, guest RAM + 128 MiB memory ceiling, no swap and bounded
threads. The agent service's cgroup alone does not necessarily contain these
separate jailer cgroups; bound/delegate their parent as well on shared hosts.
Engine detection is not a complete health check, and a bound container runtime
never silently switches stores.

## Local Linux with systemd

```sh
sudo ./bin/tt deploy all --release-dir ./bin
export TT_API_KEY="$(sudo cat /opt/ttstack/etc/api-key)"
/opt/ttstack/bin/tt config 127.0.0.1:9200
unset TT_API_KEY
/opt/ttstack/bin/tt host add 127.0.0.1:9100
```

When automating, wait for `tt status` to succeed after deployment and for the
prepared image to appear in `tt image list` before creating an environment.
Service activation and the periodic image catalog are separate readiness checks.

Use `deploy agent` or `deploy ctl` to install one role. The agent runs as root;
the controller runs as `ttstack`. Binaries go to `/opt/ttstack/bin`, state and
images to `/home/ttstack`, units to `/etc/systemd/system`. Existing role keys are
reused and persisted in `/opt/ttstack/etc/api-key` (0600). Conflicting role keys
cause an error; they are never silently replaced. Service environment files are
0600. Keep administrative keys out of commands, logs and version control.

The agent unit uses `KillMode=process` so restarting the service does not kill
its guests, and `TimeoutStopSec=360`. Deployment preserves existing
`RequiresMountsFor` and mountpoint `ExecStartPre` guards in
`tt-agent.service.d/90-ttstack-mounts.conf`. Put operator overrides in drop-ins.
A restart reconciles retained inventory; it does not automatically boot stopped
guests. A failed post-start service check returns a failed deployment, retaining
possible partial installation for inspection. It cannot be hidden by staging
cleanup or a final success message. `stop` retains disks but loses guest memory, and `delete` destroys disks.

## Distributed Linux deployment

Copy [deploy.toml.example](../tools/deploy.toml.example) into an untracked
`deploy.toml`, set addresses and resource budgets, then run:

```sh
./bin/tt deploy dist deploy.toml
```

`general.release_dir` and each agent's optional `release_dir` point to local
binaries built for that target. SSH must work with noninteractive `sudo`, including
root logins. The CLI checks paths, users, storage and budgets before remote work,
copies each binary to a private staging directory, and verifies SHA-256 before
installation. Setup scripts go through SSH stdin. Systemd, OpenRC and the Rust
manual fallback are retained. A timeout leaves the outcome unknown: inspect the
services before retrying. Custom controller `data_dir` is created and
owned by the selected controller user before service startup. This changes only
the directory entry; deployment does not recursively change database ownership
or rewrite existing state.

The shared key is reused from `deploy.toml.api-key`, imported from an existing
service, or generated. An explicit `general.api_key` replaces that sidecar; keep
it private. `tt config ADDRESS` saves address-scoped credentials in `~/.ttconfig`
using an atomic 0600 replacement. `--server` only reuses the saved key when the
address exactly matches. `--api-key` or `TT_API_KEY` supplies an explicit key.

The API is plaintext HTTP with one shared bearer key. Expose it only through the
intended trusted network or an SSH tunnel. Agent CPU/memory defaults expose all
host capacity to scheduling; set explicit budgets on shared hosts. Fixed engine
paths and networking require full namespace/root isolation for parallel native
test fleets, as described in [roadmap.md](roadmap.md).

## OpenRC hosts (Gentoo and others)

The generated OpenRC script matches the Rust reference byte-for-byte, and the
reference behavior on recent supervise-daemon versions is defective: the
service reports `status: started` while nothing listens. Two operator fixes are
required on the target until the upstream script is corrected:

1. `command_user="ttstack:ttstack"` is split into an argument list that this
   supervise-daemon rejects; edit the installed `/etc/init.d/tt-ctl` to the
   user-only form `command_user="ttstack"` (primary group is already correct).
2. `output_log`/`error_log` paths under `/var/log` cannot be created after the
   supervisor drops privileges; pre-create them owned by the service user, e.g.
   `install -o ttstack -g ttstack -m 640 /dev/null /var/log/tt-ctl.log`.

Always verify readiness by the listening socket and `tt status`, never by
service status alone. Both defects reproduce identically with the Rust release;
they were observed during live deployment on Gentoo and are recorded in
[validation/distributed-2026-10-26.md](validation/distributed-2026-10-26.md).

## Docker co-hosted with an agent

`dockerd` sets the nftables `ip filter FORWARD` chain to policy `drop` and only
allows its own bridges, which drops forwarded traffic to the `tt0` QEMU/
Firecracker bridge. Published VM ports then connect only from the agent
namespace itself. An operator running both on one host must opt in explicitly,
for example before guests are created:

```sh
iptables -I DOCKER-USER 1 -o tt0 -j ACCEPT
iptables -I DOCKER-USER 2 -i tt0 -j ACCEPT
```

These runtime rules are outside the managed tables, survive neither reboot nor
`iptables -F`, and are the operator's to keep or remove. They only short-circuit
the Docker table; the managed `nftables` NAT, outgoing-denial and isolation
semantics are unchanged. Container workloads are unaffected either way, since
Docker publishes them through its own chains.

## FreeBSD

The deployment commands intentionally reject FreeBSD. Cross-build with
`CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build -o tt-agent ./cmd/tt-agent`, copy
binaries and start services manually with private data directories and the same
shared `TT_API_KEY`. Prepare `/home/ttstack/run`, authorized PF/networking and
eligible bhyve/Jail prerequisites first. Native validation is FreeBSD 15.x only;
consult the [validation records](validation/) for the tested matrix.

## Upgrade and offline v5 conversion

The project has no formal deployment yet. Before v1.0, use the latest development
schema and recreate explicitly owned test databases when needed; older DB
compatibility is not required. Existing version checks, migrations and converter
code remain available, but do not require new schema-version increments during
development. See the [DB schema policy](development.md#db-schema-policy-before-v10).
The retained procedures below describe existing code, not a stable upgrade promise.

The optional recovery extensions migrate valid controller v5 state to v9 and
agent v6–v9 state to v10 at startup. Before upgrading, stop the service and save
an offline copy of its database and companion WAL/SHM files if present. Older Go
and pinned Rust services cannot reopen the upgraded schema. Restore a compatible
offline copy to run an older binary; never lower the schema marker manually.
The v5 offline agent converter still emits v6, which upgrades on startup.

Controller state is schema v9; older compatible controller schemas migrate on
open. Agent state must be a structurally valid native v6–v10 database: even an
empty v5 database is rejected, and
changing only `_meta.schema_version` is unsafe. The offline converter assigns
stable backup revisions while preserving old fields, host identity and bindings.

Stop the agent service before conversion and back up its metadata directory and
retained runtime/SSH metadata. Keep images, runtime paths, namespaces, host ID,
container-runtime choice and ingress/port settings unchanged. The generated
`KillMode=process` unit keeps existing guests alive when the agent stops; this
metadata conversion does not rewrite their disks or require stopping them.

For a native v5 database, run the following from the repository after building.
Use new backup/output names and keep the service stopped throughout replacement:

```sh
sudo sh <<'SH'
set -eu
tt_agent_data=/home/ttstack/data
tt_saved=/home/ttstack/data.pre-v6
test "$(systemctl show tt-agent --property=KillMode --value)" = process
systemctl stop tt-agent
test ! -e "$tt_saved"
test ! -e "$tt_agent_data/agent-v5.db"
cp -a "$tt_agent_data" "$tt_saved"
chmod 700 "$tt_saved"
./bin/upgrade-agent-v5 --source "$tt_agent_data/agent.db" \
  --output "$tt_agent_data/agent-v6.db"
mv "$tt_agent_data/agent.db" "$tt_agent_data/agent-v5.db"
for suffix in -wal -shm; do
  if test -e "$tt_agent_data/agent.db$suffix"; then
    mv "$tt_agent_data/agent.db$suffix" "$tt_agent_data/agent-v5.db$suffix"
  fi
done
mv "$tt_agent_data/agent-v6.db" "$tt_agent_data/agent.db"
SH
```

The converter holds the source service lock, reads a consistent snapshot and
publishes a new 0600 database without overwriting an existing output. Keep old WAL
and shared-memory sidecars with the old database; never leave them next to its
replacement. A failure leaves the source intact. Review the new database and
install the compatible controller/agent binaries before restarting the service;
use the same configuration and verify `tt status`, host identity and inventory.
Do not run the converter against v6, do not delete retained runtime files to
recover from a schema error, and do not downgrade an upgraded database to an
older agent. Preserve backups and inspect incomplete operations before rollback.
