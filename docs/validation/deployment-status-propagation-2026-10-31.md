# Remote deployment status propagation — 2026-10-31

A private real-systemd fixture reproduced a false-success installation result.
The controller artifact was deliberately pinned to an executable that exits
immediately. The inherited remote script evaluated its active check on the left
side of `&&`; shell `set -e` did not propagate that failure. Later cleanup and the
completion echo returned success, so the caller received an installed receipt.

The regression used the already corrected custom-directory source as its
baseline, keeping directory preparation separate from status propagation.
`TestNativeFleetRejectsInactiveControllerWithoutInstalledReceipt` failed in
7.61 seconds with stage `false_success`; private native evidence remains under
`/tmp/om-fleet-systemd-1514206283` on the authorized host. It installed no VM and
used only its own private PID1, network, mount and cgroup namespaces.

The correction makes systemd, OpenRC and manual post-start checks standalone
commands under `set -e`. A failed check stops success reporting and propagates a
nonzero installer result. Partial files, services and original host/local intent
remain retained; a failed check is not proof of rollback. Original successful
output is unchanged. The committed Rust oracle bytes were not rewritten: their
consumer applies only the three documented status-command deltas from
compatibility §7.

With `/opt/om-refactor/src/tt-deploy-result-candidate-1` and the same Open Math
fixture, the negative native case passed in 5.78 seconds, package 6.787 seconds.
The installer now returned uncertainty and preserved `installation_unknown`
without an installed receipt. The paired runner
`om-fleet-systemd-status-pair-1-20261031` completed at 01:30:44 UTC, exit 0;
it also retained the real failing baseline result rather than replacing it.

Targeted CLI race tests, vet, full default tests, full builds and Linux/FreeBSD
cross-builds passed under `tt-deploy-result-candidate-1-20261031`, exit 0 at
01:29:58 UTC. Existing services and guests were unchanged. Provider/application
readiness, initial enrollment and full Open Math delivery remain independent
requirements; this fix does not turn process-active evidence into lifecycle truth.

The combined paired source acceptance then passed at 01:32:18 UTC in
`om-fleet-systemd-final-pair-1-20261031`, exit 0: real SSH/input boundary 0.83
seconds, successful custom-directory systemd deployment 8.69 seconds and inactive
controller refusal 5.51 seconds, package 16.036 seconds. The positive case also
compared installed binary digests and checked the actual agent identity and its
explicit CPU/memory/disk budgets. This remains paired selected evidence; the
Open Math dependency must separately pin the published tt revision before a
complete Open Math profile can qualify that integrated version.
