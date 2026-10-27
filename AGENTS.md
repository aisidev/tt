# Working on tt

`tt` is the Go re-implementation of [TTstack](../TTstack) (Rust). Its purpose is
behavioral and wire parity with the Rust release, in idiomatic Go. Read
[docs/plan.md](docs/plan.md) first, then the document that matches your task.

## AI workflows

This file is the shared agent entrypoint. Reusable workflows live in
`.agents/skills/`: `$x-review` reviews, `$x-fix` fixes recorded findings,
`$x-commit` reviews and commits intended work, and `$x-overhaul` combines review,
fixes and commits. These skills require explicit invocation; ordinary requests
do not implicitly authorize commits. See [docs/ai-workflow.md](docs/ai-workflow.md)
for scopes, ownership, validation and commit rules. Read
[docs/ai-review.md](docs/ai-review.md) for review evidence and subsystem checks.

## Scope and ownership

- `cmd/{tt,tt-ctl,tt-agent,upgrade-agent-v5}`: startup wiring only.
- `model`, `api`, `capability`, `backup`, `guestconfig`, `sshboot`,
  `auth`, `runcmd`: shared, platform-independent behavior.
- `internal/engine`, `netcfg`, `storage`: host-side implementations, selected by
  host OS at runtime.
- `internal/agent`, `internal/ctl`, `internal/cli`: the three components.

The reference implementation normally lives at `../TTstack`. The revision pinned
in the plan defines expected behavior, subject to the documented deviations in
compatibility §7; the sibling checkout's current HEAD is not automatically the
oracle. If that checkout is unavailable, use committed fixtures and documented
contracts, and label source-dependent parity conclusions unverified.

## Ground rules

- No new features. Parity only: same API, same JSON, same paths, same printed
  output, same status codes. Anything not in the Rust release does not belong here.
- Compatibility is normative: [docs/compatibility.md](docs/compatibility.md)
  records every wire rule, identifier and deliberate deviation. Changing a field,
  path, default or message requires updating that document in the same commit.
- Before v1.0, retain DB versioning infrastructure but do not increment schema
  versions for development changes or require compatibility with older development
  databases. Update the current schema and tests in place; follow the
  [DB schema policy](docs/development.md#db-schema-policy-before-v10).
- Dependencies are a closed set (see [docs/development.md](docs/development.md)).
  Adding one needs an explicit, written reason.
- Reservations, repository contents and state formats are never silently
  "improved"; conservative behavior (retaining resources on unknown outcomes) is
  part of the contract.

## Development and verification

- For phased delivery, follow [docs/roadmap.md](docs/roadmap.md); each phase ends with
  `gofmt -l .` clean, `go vet ./...`, `go test ./...`, `go build ./...` and one
  commit. Do not start the next phase on a red build. Phases 0–7 are complete;
  routine maintenance uses the scoped gates in the AI workflow, without version
  bumps or commits unless requested.
- This shared workstation uses `GOMAXPROCS=2` and Go `-p 1`, with checks run
  sequentially. Do not set up local VM/container/network infrastructure. Real
  tool/disk integration, race suites and Rust builds run on authorized hosts or
  CI; do not enable native-test environment variables for normal local tests.
- Rust oracle builds use the official latest stable Rust; this repository
  defines no compatible Rust version range or minimum compiler. The pinned
  reference source revision remains the behavioral oracle.
- Port the Rust unit tests that encode contracts; keep their intent and names
  recognizable. Add golden fixtures for wire types instead of asserting by hand.
- Check lifecycle changes for partial failure and retries. A timeout does not
  prove that creation failed. Stop retains disks but not memory; delete is
  destructive. Process readiness is not application readiness.
- Never trust a stale PID file; always validate process identity by marker.
- Keep repository documentation, instructions and commit messages in English.
  Respond to the user in their language.

## Live systems

Functional VM testing requires an authorized host and is bounded: no stress
testing, keep well under half of host CPU/memory, never disturb existing guests,
and clean up only what the task created. Administrative keys stay out of logs and
reports. Native FreeBSD validation is FreeBSD 15.x only; state that limit
when reporting results.

Existing validation reports and harness paths are evidence, not authorization
to reuse a host or mutate its resources. Follow the isolation requirements in
[docs/roadmap.md](docs/roadmap.md#live-validation-order-and-isolation).

## Code review rules

- Report concrete triggers and observable consequences, supported by current
  code and the pinned contract. Style preferences and speculative improvements
  are not parity defects. Check callers and guards before recording a finding.
- Unknown creation/deletion outcomes must retain ownership and reservations;
  recovery must not adopt or destroy resources based on a name or stale PID alone.
- A passing mock, cross-build or historical native report does not prove current
  native behavior. State what was checked, what was skipped and what remains
  unverified. Record confirmed outstanding findings in [docs/audit.md](docs/audit.md)
  when using the project review workflows.

## Repository layout notes

- `docs/` is the maintained description; keep it small and linked, not duplicated.
- `tools/` holds operator-facing templates such as `deploy.toml.example`.
- Do not commit keys, databases, images or generated binaries.
