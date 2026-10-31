# One-time controlled enrollment grants — 2026-10-31

This change adds an explicitly selected private grant store to controlled host
enrollment. It does not replace authority/epoch, activated-plan, physical scope,
exact host identity or workload non-adoption checks. The private token is carried
only in `X-Fleet-Enrollment-Token`; the public request and cached reply contain no
secret. Compatibility owns the private format and deliberate extension.

## Persistence and HTTP boundaries

Race tests cover original initialization/issuance replay, exact command claim,
foreign-key reuse, wrong epoch/request, original completed receipt, directory
identity, missing owner/catalog/row/claim and disappearing completed-result data.
The independently anchored owner count prevents removed admission rows from
becoming unused permission. A separate receipt digest prevents a lost completed
receipt from minting a new registration result. Missing/incomplete pairs retain
unavailable evidence. Issuance stores only token digests in the bounded catalog;
values remain in private token files.

Actual controller HTTP tests require invalid/missing grants to refuse before
contacting the agent. Successful original cache replay remains byte-identical;
a lost command-cache publication reconciles the independent original business
receipt rather than generating a new UUID. Another command key cannot reuse the
grant. The ordinary legacy profile still performs its original checks. A separate
regression changes the external epoch during the final cold agent-info request;
the membership write guard rejects the stale observation and stores no member.

## Selected real-service integration

The immutable tt development snapshot is
`/opt/om-refactor/src/tt-enrollment-grants-accepted-5`. It was paired explicitly
with `/opt/om-refactor/src/om-fleet-enrollment-candidate-5`; this is selected source
pairing, not an assertion that an older published module contained the feature.
`om-fleet-enrollment-paired-5-20261031` completed at 02:01:34 UTC, exit 0. The native
race case passed in 9.99 seconds, package 10.996 seconds.

The fixture installs actual controller/agent services in private systemd/PID,
mount, network and cgroup domains. It starts fenced, obtains each original fence,
reads every frozen physical inventory page, verifies complete count/digest and
zero processes, then drives actual reads/mutations activation with original
receipt replay. A service-owned local issuer initializes and issues its grant.
The operator client enrolls the exact declared host once, replays its original
receipt after retiring the local token copy, rejects a different key using the
consumed token, and verifies actual one-host membership at the original endpoint.
No existing VM/disks are adopted or created; existing outer services are untouched.

The final source also runs targeted command/controller/CLI race tests, full vet,
default tests/builds and Linux/FreeBSD cross-builds in
`tt-enrollment-grants-check-5-20261031`. Its actual terminal result must be recorded
before qualification. Full Open Math verification must independently pin the
published tt module with no source override. This proof does not complete guest
bundle, application/link deployment, normal reclamation or whole-project goals.

The immutable final tt source passed all targeted race tests, vet, full default
tests/builds and Linux/FreeBSD cross-builds at 02:01:52 UTC in
`tt-enrollment-grants-check-5-20261031`, exit 0. No schema marker or dependency
version changed. Only new private external grant files and optional controlled
enrollment/deployment fields were introduced; compatibility documents them.
