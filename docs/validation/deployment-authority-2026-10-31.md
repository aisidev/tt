# Distributed receiver authority selection — 2026-10-31

Controller and agent deployment tables now optionally pass the existing
`--authority-file` receiver option. Paths must be canonical and outside database,
image/runtime and installation roots. Before upload or role setup, deployment
observes a pre-existing private regular file without following its final symlink.
It does not initialize, replace, upload, chown or advance that authority or its
sidecars. Absent selection preserves the legacy profile and existing template
fixtures. Compatibility §7 and the deployment contract own this explicit extension.

Negative CLI tests reject a restored-database/payload location and unsafe paths
before remote work. A selected missing controller authority stops before any
binary upload or service setup. Original default Rust template fixtures remain
unchanged. CLI race tests, vet, full default tests, builds and Linux/FreeBSD
cross-builds passed under `tt-deploy-authority-accepted-2-20261031`, exit 0 at
01:44:30 UTC. The immutable source was
`/opt/om-refactor/src/tt-deploy-authority-accepted-2`.

The paired Open Math snapshot `/opt/om-refactor/src/om-fleet-authority-candidate-3`
provisioned separate authority files/directories outside both restored stores.
The unprivileged controller could read/write only its selected authority scope;
the root agent used its own file. Real SSH/SCP and private systemd installed both
services. Both advertised the exact authority/epoch and remained `fenced` after
installation. Original controller and host fencing commands then committed their
distinct receipts, and exact replay preserved bytes. An enrollment before serving
activation remained refused. The selected native race case passed in 9.28 seconds,
package 10.285 seconds, at 01:44:20 UTC in
`om-fleet-authority-paired-2-20261031`, exit 0.

This selected development pairing used an explicit test-only source override;
it does not claim that an older module already included this extension. It
establishes neither initial one-time host enrollment nor permission to schedule
VMs, activate mutations or bypass physical inventory review. Existing deployed
services and guests were untouched; the manager, SSH keys, network, filesystem,
PID and cgroup domains were task-private and bounded to two CPUs/four GiB.
