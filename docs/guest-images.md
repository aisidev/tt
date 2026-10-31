# Guest images

`tt image recipes` lists the twelve recipes from the pinned Rust release.
`tt image create NAME --image-dir DIRECTORY` builds on the machine running the
CLI; it does not upload to an agent. `tt image list` queries the controller's
current agent catalogs. Prepare the image directory before QEMU image creation.

| Recipes | Behavior |
|---|---|
| `alpine`, `debian`, `ubuntu`, `rockylinux` | Pull pinned container tags and add local aliases; base images need a long-running workload |
| `nginx`, `redis`, `postgres` | Pull workload container tags; PostgreSQL requires its own initialization settings |
| `alpine-cloud` | Alpine 3.21.7 NoCloud qcow2, pinned SHA-512 |
| `debian-cloud`, `ubuntu-cloud` | Latest Debian 13 / current Ubuntu 24.04 image, verified against upstream SHA-512 / SHA-256 manifests |
| `fc-alpine` | Pinned kernel and Alpine userspace, 128 MiB ext4 root; boot/network check only, no SSH |
| `freebsd-base` | Jail root from the host's exact RELEASE series; native validation is recorded on FreeBSD 15.1 |

```sh
sudo mkdir -p /home/ttstack/images
sudo ./bin/tt image create alpine-cloud
sudo ./bin/tt image create all --engine qemu
```

`all` can be filtered by recipe engine. Without a filter it skips Jail on other
systems and Firecracker on FreeBSD. Container recipes prefer Docker, then Podman.
QEMU recipes need `qemu-img`; raw downloads are converted to qcow2. Downloads are
staged, checksum-verified and atomically published. A failed download or checksum
never replaces the prior image. Existing QEMU images must be nonempty qcow2 disks.

The Firecracker recipe needs Linux mount privileges, `dd`, `mkfs.ext4` and `tar`.
It unmounts even after population failure; an uncertain unmount retains the mount
and staging rootfs for inspection. Do not remove a retained staging directory
until its mount is resolved. A custom Firecracker image must provide the guest
configuration and SSH support required by the workload; `fc-alpine` is not such
an SSH image.

`freebsd-base` uses native `fetch` and `tar`, verifies essential base files and
writes a completion marker before publication. It refuses an incomplete existing
root and non-RELEASE hosts. Image preparation does not by itself establish working
Jail networking or supported guest resource enforcement.

File and zvol storage layouts and runtime paths follow
[compatibility.md](compatibility.md). ZFS datasets and image import are operator
preparation, separate from these local file recipes.

## Firecracker guest configuration

The API's `guest_config` is a map of simple file names to UTF-8 string contents;
CLI `--guest-config FILE` reads that JSON object, with a 512 KiB input limit.
There may be at most 32 files and 64 KiB of combined filename/content bytes.
Names are 1–128 ASCII letters/digits or `-_.`, with no leading dot or directory
separator. The managed `ttstack-ssh.sh` seed is reserved; its file/count/size also
uses the same budget. Other engines reject this caller-facing configuration.

The agent builds a 4 MiB ext4 `guest-config.ext4` drive, attached readonly as the
second virtio disk. The prepared init mounts it with readonly/nosuid/nodev/noexec
options. Ordinary files are data, never an installation script contract. Only a
prepared SSH init explicitly executes the reserved managed seed as described in
[ssh.md](ssh.md). Values and configuration digests remain opaque to scheduling.

## Storage and recovery

With `--storage file`, `--image-dir` and `--runtime-dir` are directories. QEMU
bases are standalone qcow2 files. A Firecracker base is a directory containing a
regular `vmlinux` and an unpartitioned ext4 `rootfs.ext4`. Runtime clones are
`clone-VM_ID`; Firecracker keeps persistent hard links to its file root inside the
jail. Keep the root and jail on the same filesystem. File backup qualification is
separate from provisioning's optional reflink optimization.

With `--storage zvol`, both arguments are **dataset names**, not device paths.
Prepare an existing pool and parent datasets with active absolute mountpoints,
for example `tank/ttstack/images` at `/srv/tt-images` and
`tank/ttstack/runtime` at a short path such as `/srv/tt-run`. The software does not
create pools or import images. Use new owned image names for imports and never
write to a volume used by a guest.

A QEMU/bhyve image is a root zvol sized to the raw disk. For QEMU, inspect the
source's virtual capacity, create an unused volume of at least that capacity,
wait for its device, then convert qcow2 to raw with `qemu-img convert -n -f qcow2
-O raw SOURCE /dev/zvol/DATASET`. Do not copy qcow2 container bytes directly into
a zvol expected to contain a raw disk. Bhyve requires a prepared FreeBSD raw disk
and reports its capacity before placement.

A Firecracker ZFS image has this layout:

```text
tank/ttstack/images/fc-app         filesystem dataset containing vmlinux
 tank/ttstack/images/fc-app/rootfs volume containing unpartitioned ext4
```

Create the parent filesystem, copy the kernel into its mountpoint, create a
sparse `rootfs` zvol with `volmode=dev`, and copy the prepared ext4 image to that
new volume. Match its capacity; if the base volume is larger, run offline
`e2fsck`/`resize2fs` before use. Leave `rootfs.ext4` **absent** in the parent
image dataset: each clone gets a symlink to its own cloned volume. Readonly
kernel/config files may be copied across datasets, while the jail receives a
private block-device node for its own root; host device ownership is unchanged.

Bases use fixed `@ttsnap` snapshots, recursively for Firecracker's kernel/root.
Editing a base does not refresh that snapshot; import another revision under a
new name. Stop retains runtime disks/datasets; deletion removes only the owned
clones and runtime metadata. Partially created clones remain associated with
failed VM records for inspection and retryable cleanup. Do not remove PID, disk,
SSH seed or sandbox metadata to force recovery, or destroy a live clone by hand.

Keep data and `/home/ttstack/run` persistent across service restarts. Runtime
paths must remain short enough for Unix sockets. Mount guards belong in systemd
drop-ins (`RequiresMountsFor` plus `ExecStartPre=... mountpoint -q ...`); startup
must fail when required mounts are missing. Quotas, free-space headroom and the
agent's logical disk budget are distinct controls. See [deployment](deployment.md)
and [disk backup](disk-backup.md) before changing retained state.

## Networking and published ports

Linux QEMU/Firecracker use TAPs on `tt0`, `10.10.0.1/16`, with host-local guest
addresses and nftables NAT. **A co-hosted Docker daemon drops that forwarded
traffic by default; see [deployment](deployment.md#docker-co-hosted-with-an-agent)
for the operator opt-in.** The seeded `dns_nameservers` are applied by most
cloud images, but Alpine NoCloud's cloud-init does not write `/etc/resolv.conf`:
its guests have IP-level egress through the host masquerade while name
resolution fails inside the guest. The network seed is identical to the
reference; this is an Alpine image property, not a fleet difference. Addresses can repeat on different hosts; there is no
cross-host private guest network. Published TCP mappings belong to the agent host,
not the controller, and target only packets addressed to that host. Traffic for
another destination that happens to use the same numeric port keeps its destination.

For VM engines, published ports are an ingress path for another machine. Linux
PREROUTING DNAT does not provide host-local loopback access to that mapping; from
the agent namespace use the guest's direct IP/port. Containers use their runtime's
publication behavior. Provider routing/firewalls and public ingress are operator
prerequisites. Managed SSH advertises only an explicitly configured endpoint;
see [ssh.md](ssh.md), including disjoint port ranges for agents sharing ingress.

**Non-isolated guests can reach wildcard-bound management listeners through
`10.10.0.1`.** Bind management to the intended trusted interface/loopback, protect
its bearer key and choose network isolation where required. Outgoing denial alone
does not isolate the host or peer guests. Isolation for Linux QEMU/Firecracker
also restricts source identity, guest peers, guest-initiated host/private/link-local
access and IPv6, while allowing public IPv4 egress and replies to inbound traffic.
Neither policy is implemented by Docker/Podman; bhyve has egress denial only.

FreeBSD hosts, validated on FreeBSD 15.x only, require a prepared bridge and enabled PF with
`rdr-anchor "ttstack/*"` and non-quick `anchor "ttstack/*"` hooks; outbound NAT is
provisioned separately. Without that NAT, guests whose base contains a public
`resolv.conf` cannot resolve, and their sshd spends tens of seconds in resolver
retries before answering new connections. Use generous SSH client timeouts or
provision outbound NAT; the behavior is identical for the reference. Merge hooks into an owned configuration rather than
replacing a shared firewall. For tests, use a dedicated VNET with bhyve/child-jail
permissions and a matching devfs ruleset; [native tooling](../tools/native/README.md)
records the verified setup. Fixed bridges, firewall names and run paths require
network **and** filesystem isolation when running multiple agents on one host.

The optional private configuration restart protocol in
[compatibility §17](compatibility.md#17-optional-private-guest-configuration-start)
can replace the caller file set before a physically stopped Firecracker VM starts.
It requires a preexisting configuration-drive reservation, current durable command
identity and independently negotiated agent support. It preserves the original
managed SSH seed. The protocol does not execute file contents or prove the image
contains a consumer for them; prepared runtime/image assembly remains separate.
