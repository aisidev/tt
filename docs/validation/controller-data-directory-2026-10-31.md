# Distributed controller data directory — 2026-10-31

The Open Math isolated fleet fixture reproduced a configured-state-directory
startup failure with the pinned Go release `d4357a3`. `controller.data_dir` was
`/var/lib/om-fleet-native/controller`, with a non-root controller service user.
The installer prepared only the default home directory. The actual controller
journal reported permission denied while creating the configured directory.
The original private evidence remains under
`/tmp/om-fleet-systemd-4279778048` on the authorized validation machine.

The correction passes the configured path into directory preparation and assigns
that directory entry to the controller's user/group before startup. Ownership is
not recursive: existing database contents, formats, identifiers and timestamps
are not rewritten or removed. Default-directory rendered fixtures remain exact.
The custom-directory reference-Rust behavior is unverified because that checkout
was unavailable; the deliberate Go correction is documented in compatibility §7.

The patched immutable tt source snapshot was
`/opt/om-refactor/src/tt-controller-deploy-state-candidate-1`. It was paired with
`/opt/om-refactor/src/om-fleet-systemd-native-candidate-11` using the explicit
fixture-only source override. Actual SSH/SCP, root host intent, private PID1
systemd, non-root controller startup, root agent startup and both authenticated
HTTP observations passed. The selected race test took 8.85 seconds, package
9.857 seconds, and `om-fleet-systemd-native-candidate-11-20261031` exited 0 at
01:22:46 UTC. This pairing is selected native evidence, not an assertion that an
unmodified pinned module contained the patch.

The tt targeted CLI race suite passed, including original default Rust template
fixtures. Vet, full default tests and full builds passed under
`tt-controller-deploy-state-candidate-1-20261031`, exit 0 at 01:21:34 UTC. Linux
amd64 and FreeBSD amd64 cross-builds also passed. No existing agent, controller,
guest or host network was modified; the private native manager had its own mount,
network, PID and cgroup namespaces with a two-CPU/four-GiB ceiling.

The independently observed installer failure-propagation issue is not closed by
this directory correction: the inherited shell can report completion after a
failed service check. Provider readiness, initial enrollment and successful
application/guest deployment remain separate Open Math integration gates.

## Concurrent upstream integration

The remote history was rewritten while this repair was being prepared. The new
`cdf4612` lineage already contained the durable storage/recovery backports and
public-package adaptation. Only the intended directory repair was rebased onto
that lineage; the new permanent-lifetime/explicit-SSH defaults and other parallel
changes were preserved. A documentation conflict retained both compatibility
entries rather than choosing one side.

The rebased immutable source `/opt/om-refactor/src/tt-controller-deploy-state-accepted-2`
passed targeted CLI race tests, vet, full default tests, full builds and both
cross-builds at 01:27:13 UTC in
`tt-controller-deploy-state-accepted-2-20261031`, exit 0. Paired real SSH/systemd
acceptance then passed in 10.62 seconds under
`om-fleet-systemd-native-rebased-1-20261031`. This revalidates the fix against the
actual integrated source, rather than transferring the earlier native claim to
new upstream code without checking it.
