# Private guest configuration start — 2026-10-31

The optional Open Math OS integration in compatibility §17 delivers a new caller
file set before a physically stopped Firecracker VM launches. It is explicitly
outside pinned Rust parity; legacy request formats remain intact.

## Exact source and checks

On authorized `root@192.155.106.13`, immutable snapshot
`/opt/om-refactor/src/tt-guest-bootstrap-accepted-1` completed
`tt-guest-bootstrap-check-1-20261031` at 03:01:11 UTC, exit 0. The local and remote
source inventories matched 363 files, digest
`d237cc6f4d6221d222f9c7a2282c716be18b3043547a296057fe71811cf161ec`.
The inventory includes every file except `.git`, `docs`, `.agents`, `bin`, `dist`
and Python caches, with sorted relative-path/content hashes encoded as compact
sorted JSON before hashing. Documentation and generated cross-build outputs do
not represent additional accepted code.

Formatting, full vet/staticcheck, full race suite, full build, and static Linux/
FreeBSD amd64 cross-builds passed sequentially with GOMAXPROCS=2 and Go -p 1.
The selected race checks then used actual mkfs.ext4/debugfs in private temporary
paths. They verified exact replacement-file bytes and committed caller digest
before the engine callback, original running replay without another launch,
different-input refusal, physical-state refusal before disk publication and old
disk retention after failed formatting. Those engine callbacks are controlled
fixtures; no claim of a real Firecracker guest boot is made by this phase.

Controller tests exercised real local HTTP against an explicit agent fixture:
an old/unnegotiated host received no start, while the negotiated route received
exact private files and the original VM-specific durable command. A separate
mutation-boundary test confirmed that request cancellation does not cancel
already admitted work, original metadata survives and a superseded watermark
refuses before the callback. Existing real ext4 guest-configuration tests also
passed, including private disk permissions and failed-build retention.

An initial compile candidate exposed an incorrect helper context reference;
its subsequent correction excluded the metadata-free legacy reconciliation loop.
A native attempt with an incomplete PATH could not find mkfs.ext4 and failed
before disk work. Adding the installed standard sbin directories corrected the
runner configuration; no assertion or production capability gate was relaxed.

## Remaining integration

This is host/channel support. Platform credential preparation, create/start/
restore integration, prepared runtime/image assembly, guest consumption and an
authorized outward relay round trip require their own evidence. A configuration
write is not application readiness. Existing services, guests and host networking
were not changed. No paid model call or credential publication occurred.
