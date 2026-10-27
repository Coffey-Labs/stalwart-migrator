# Design: the route to Stalwart 1.0

Status: proposal, 2026-10-26. Nothing here is implemented yet. ARCHITECTURE.md
stays the record for the 0.15 → 0.16 migration; this document adds the 1.0
route on top of the same engine.

## 1. Goal

One supported route from any Stalwart release this tool already handles to
1.0.x:

```
0.15.5 ──(existing major path)──▶ 0.16.27 ──(1.0 copy)──▶ 1.0.x
0.16.x ──(existing patch path)──▶ 0.16.27 ──(1.0 copy)──▶ 1.0.x
```

Stalwart 1.0 has a new database layout and can't be upgraded in place. The
data has to be copied into a new, empty store. Stalwart Labs publishes a tool
for that copy, `stalwart-migrate` (stalwartlabs/stalwart-migrate). It reads
v0.16 without writing to it, resumes after an interruption, verifies its
result and offers a rollback. This tool doesn't replace it. It does the work
around the copy: getting to 0.16.27, checking the server first, rehearsing on
a copy, running the copy and the cutover as one checkpointed run, and
validating the result the way it does for 0.16.

The name clash is unfortunate: our binary is also called `stalwart-migrate`.
In this document, **upstream's tool** means Stalwart Labs' copy tool and
**this tool** means stalwart-migrator.

## 2. Why build on upstream's tool

A rehearsal on 2026-10-26 took a seeded 0.16.27 server (two users, mail in
several folders, flags, a calendar event, a contact, an active Sieve script,
an app password) to 1.0.0 with upstream's tool. `prepare` asked four questions
and reported 48 warnings, all of them settings with no 1.0 equivalent. The
final run took 31 seconds with 0 errors, and upstream's verification passed. A
JMAP snapshot before and after was identical for account ids, mailboxes,
message ids, threads, keywords, events, contacts and Sieve scripts. The only
change was a keyword 1.0 adds to every message: `$hasattachment` or
`$hasnoattachment`.

A second rehearsal used PostgreSQL and more data: five accounts with 2,000
messages each (400 with attachments, four folders, threads, flags), 200
events, 300 contacts and five Sieve scripts. The first incremental `run`
copied all 10,000 messages in 3.5 seconds while 0.16.27 kept serving.
Changes made afterwards (new mail, flags, deletions) were picked up by a
second incremental run, and changes after that by `run --final`, which took
31 seconds, 30 of them spent confirming the source had stopped. Upstream's
verification passed, and the same per-account snapshot of 10,090 messages
matched apart from the keyword above.

A copy engine that already works, keeps ids, and has its own rollback is not
something to rebuild. This tool runs it as a separate, unmodified program
and never links or patches it.

## 3. What operators need that the copy doesn't cover

1. **Getting to 0.16.27.** Upstream's tool accepts exactly 0.16.27. Older
   0.16 releases need a patch bump first, and 0.15 needs the full 0.16
   migration. Both already exist in this tool.
2. **Knowing what will change before anything does.** Some things only show
   up late: OAuth clients that need registering, OIDC behavior, reverse-proxy
   headers, Enterprise-only settings on a Community build, the disk needed for
   a second copy.
3. **A rehearsal against their own data with no downtime** (§6).
4. **A cutover**: point the service at the new binary, configuration and store,
   for systemd and Docker, while keeping the 0.16 store untouched to fall back
   to.
5. **Validation they can read**: the same per-account comparison this tool
   already runs after a 0.16 migration.
6. **Running unattended.** Upstream's `prepare` needs a terminal. Unattended
   runs need its recorded answers.

## 4. Plan

`internal/plan` gains a 1.0 boundary. Crossing it adds two phases:

| Phase | What it does |
|---|---|
| `preflight` | Existing checks, plus the 1.0 checks (§5) |
| `hop` | Only when the source isn't 0.16.27: the existing patch or major path up to 0.16.27, as its own checkpointed sub-run |
| `backup` | Existing: settings and principals dumps, preserved binary and service definition. The 0.16 store itself is never written by the copy |
| `copy` | Upstream's tool: `prepare` (or a check of recorded answers), then incremental `run`s while 0.16 serves mail |
| `cutover` | Stop 0.16, upstream `run --final`, install 1.0, rewrite the service, start 1.0 |
| `validate` | Existing validation plus the 1.0 comparison (§8) |

By default the run **stops after `hop`**, so the operator can confirm 0.16.27
works before the 1.0 copy starts. `--through 1.0` runs both hops in one go.
Two major boundaries in one unattended run is a choice the operator makes on
purpose, not a default.

## 5. Preflight additions

Read-only, like every preflight check. Each one names the setting and says
what changes.

- **Version:** exactly 0.16.27 for the copy. Anything older plans a hop.
- **Disk:** room for a second data store. With an external blob store that
  1.0 reuses, only the data store is duplicated. With blobs inside the data
  store, it's the whole thing.
- **Store backend:** RocksDB, SQLite, PostgreSQL and MySQL are in scope for
  the first release. FoundationDB needs upstream's `-fdb` build and is
  reported, not attempted. A SQLite store on NFS is refused while 0.16 runs,
  which matches upstream's documented limitation.
- **Cluster:** single node only in the first release. Clusters are refused
  with an explanation; 1.0 also requires a Redis in-memory store for them.
- **Edition:** the official 1.0 images and binaries are built with the
  `enterprise` feature, and 1.0 stamps each store with the edition of the
  build that created it. A store made by the official build can't be opened
  by a build compiled without `enterprise`, and the reverse is also true.
  Preflight asks which 1.0 build the operator will run (official by default)
  and selects upstream's matching tool build (`-oss` for builds without
  `enterprise`). It says plainly that this choice decides which builds can
  open the new store.
- **Enterprise-only settings on a Community target:** listed by object, since
  upstream's `-oss` tool can't store them.
- **Things that change behavior** (upstream's `prepare` asks about each):
  OIDC providers and client registration, `useXForwarded`, `counter_get` in
  expressions, the LLM spam classifier and AI models, SIG(0) DNS servers,
  Azure blob authentication, custom roles. Preflight lists which ones the
  server has, so the operator knows the questions before the window.
- **Web interface:** 1.0 serves only web interface v2. A custom bundle URL
  is reported.
- **`stalwart-cli` plans:** if the operator manages settings as code, they
  need converting (upstream ships `cli-plan/convert_plan.py`). Reported,
  not run.

## 6. Rehearsal with no downtime

`stalwart-migrate rehearse --to 1.0` uses upstream's incremental mode, which
reads a running 0.16 server:

1. Stage upstream's tool (§7) and a 1.0 binary into the rehearsal's work
   directory.
2. Run upstream's `prepare` in a pseudo-terminal, accepting the recommended
   answers. Record every question and answer in the rehearsal report, so the
   operator sees the real questions in advance.
3. Run one incremental `run` from the live 0.16 server into a scratch store in
   the work directory.
4. Start a throwaway 1.0 on the scratch store, bound to loopback on unused
   ports, with the recovery administrator set through the environment.
5. Compare the live 0.16 server and the scratch 1.0 per account (§8). Report
   any differences.
6. Stop the throwaway server and delete the scratch store, unless
   `--keep-artifacts`.

Nothing in the rehearsal writes to the live store or touches the live
service. It needs the same disk headroom as the real copy, and preflight
checks that first.

## 7. Staging upstream's tool

- Download the pinned release archive (or pull the pinned image for Docker
  deployments) and check it against the release's published checksums; for
  images, verify the cosign signature upstream publishes.
- Record the exact version and digest in the run's checkpoint. A resumed run
  uses the same build or refuses.
- Pin per release of this tool. The supported pair is tested together.
- Never modify it, never pass undocumented flags, and treat its output and
  reports as data: parse its exit codes and the documented `status` and
  `report` output, and keep the raw logs in the run directory.

## 8. Validation for 1.0

The existing `validate` package compares a preflight snapshot with the
migrated server. For 1.0 it gains the comparison the rehearsal above used,
per account:

- mailbox names, roles, totals and unread counts
- message ids, subjects, keywords, thread ids and mailbox membership
- calendar event ids, contact card ids, Sieve script names and the active
  script
- principals, domains and DKIM keys at server level

Known, documented 1.0 differences are listed in one table in the code and
reported as information, not failures. The first entries are the
`$hasattachment` and `$hasnoattachment` keywords. Upstream's own `verify` result is included in the
report, and a mismatch in either fails the run.

After cutover the report prints what users will notice: OAuth clients sign
in again, CalDAV, CardDAV and WebDAV clients fetch their data once more, and
search results stay incomplete until the index rebuild finishes. Validation
reports the progress of that rebuild.

## 9. Cutover and falling back

- **systemd:** install the 1.0 binary beside the old one, write the 1.0
  `config.json` for the new store, and rewrite the unit with the existing
  `internal/cutover` code. The 0.16 binary, configuration, unit and store stay
  as they are.
- **Docker:** a new image tag and a new data volume mapped at the path the
  target configuration names, using the existing container cutover and
  upstream's `--target-path-map` where the paths differ.

**Falling back before 1.0 has served users:** restart 0.16 on its untouched
store. The report prints the exact commands. With a shared external blob
store, this holds only until upstream's final run has replaced the spam model
keys, and the report says so. **After 1.0 has served users:** upstream's
`rollback` into a new 0.16 store. This tool prints the command and does not
run it. That matches the existing non-goal: this tool isn't a recovery tool
(ARCHITECTURE.md §4.8).

## 10. Unattended runs

`run` accepts a recorded `answers.toml` (from a rehearsal or an earlier
`prepare`) and runs upstream's `prepare --non-interactive` to check it. If a
question is still open, the run stops at preflight and says which. Without
recorded answers and with a terminal attached, upstream's `prepare` runs
interactively in the operator's terminal.

## 11. Out of scope for the first release

- clusters and FoundationDB (reported by preflight, not attempted)
- switching an existing 1.0 store between editions
- Windows (upstream's tool doesn't support it natively)
- running upstream's `rollback` (printed, never run)
- the extra recovery boot on the 0.15 → 0.16 path, which is still waiting on
  field reports (issue #1). Chaining doesn't change it.

## 12. Milestones

1. `plan`: the 1.0 boundary, the `hop` and `copy` phases, `--through`.
2. `preflight`: the checks in §5.
3. Staging upstream's tool, with checksum and signature verification.
4. `rehearse --to 1.0` (§6), including the comparison in §8.
5. `run` for systemd: copy, cutover, validation.
6. `run` for Docker.
7. Documentation: the upgrade guide on docs.ihasmail.org, status.md and
   known problems, then a release.

## 13. Open questions

- **Distribution of upstream's tool on systemd hosts:** the release archive
  (no Docker needed) or the image (matches paths and ownership). Proposal:
  the archive for systemd, the image for Docker.
- **Which 1.0 patch release to target** when 1.0.1 and later ship: follow
  upstream's latest by default, with `--target` to pin, as this tool does
  for 0.16.
