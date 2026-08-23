# stalwart-migrator — Architecture

Status: design, no implementation yet.
Scope: upgrade a Stalwart Mail Server in place from **0.15.5** to the current
latest release (**0.16.14** as of 2026-08-19) with no data loss, a working
rollback at every step, and an automated post-migration validation pass.

## 1. Why this isn't a thin wrapper

Stalwart does not ship an automated upgrade tool today (planned for 1.0,
targeted H1 2026, not yet released). 0.15.5 → 0.16.x is a **major** boundary,
not a patch bump, and it is unusually dangerous to automate naively:

- The v0.15 → v0.16 config model changes completely: multiple TOML files plus
  DB-resident settings collapse into one `config.json` that describes only the
  datastore connection, with everything else moved into JMAP-managed objects.
- Account names change from bare usernames to full email addresses; DAV URLs
  change (`/dav/cal/alice` → `/dav/cal/alice%40example.com`).
- **On first v0.16 start, the server irreversibly deletes** all directory
  records (users/groups/domains/tenants/OAuth clients), all settings, DMARC/
  TLS/ARF reports, pending tasks, telemetry, spam training samples, and quota
  counters. Mail/calendar/contact data is untouched, but everything else is
  gone unless captured first.
- Migration requires a manual "recovery mode" boot of the new binary, then an
  external tool (`stalwart-cli apply`) replays a converted settings snapshot
  into it over HTTP while it's up in that special mode — a multi-process,
  multi-terminal, stateful procedure with no built-in resumability.
- In a cluster, every node must be stopped before migration starts; one node
  left on v0.15 corrupts the shared store.
- Real-world failure mode already reported in the wild: post-migration WebUI
  login breaks because the UI now requires HTTPS via `defaultHostname`, not
  plain IP access — a config/DNS issue, not a data issue, but it reads as
  "the migration broke everything" to an operator.
- Not all settings migrate automatically: SMTP listeners, routing, rate
  limits, spam rules, and auth backends are explicitly **not** carried over
  by Stalwart's own conversion script and must be recreated or replayed from
  a separately captured snapshot.

None of this is exotic — it's exactly what Stalwart's own
[`UPGRADING/v0_16.md`](https://github.com/stalwartlabs/stalwart/blob/main/UPGRADING/v0_16.md)
and `resources/scripts/migrate_v016.py` already do. This project's job is to
turn that fragile, manual, two-terminal runbook into a single supervised,
checkpointed, reversible operation — and to keep working as new releases land
on top of 0.16.x, most of which (0.16.1–0.16.14, per changelog) are pure
patch/feature releases with **no** schema migration, i.e. a binary swap +
smoke test, not a full migration.

## 2. Design goals / non-goals

**Goals**
- Zero data loss for mail, calendar, and contact content (the one thing
  Stalwart itself guarantees is untouched — everything else is on us).
- Every phase has a defined, tested undo. Nothing destructive happens until
  a verified backup exists.
- Fully automated happy path; the operator answers a preflight confirmation
  once, then watches (or walks away and checks the report).
- Resumable: if the process dies mid-migration (crash, SSH drop, OOM), a
  re-run picks up from the last completed checkpoint instead of redoing or,
  worse, double-applying destructive steps.
- Works across the deployment shapes Stalwart actually supports: systemd +
  bare binary, Docker/Compose, and single-node vs. cluster — with embedded
  (RocksDB/SQLite) or external (PostgreSQL/MySQL/FoundationDB) stores.
- Extensible to future major boundaries (0.16 → 1.0 and beyond) without a
  rewrite: version-boundary logic is pluggable, not hardcoded into the core
  engine.

**Non-goals**
- Not a general Stalwart config management tool (no drift detection,
  no day-2 ops beyond the migration window).
- Not a replacement for routine backups — it *produces* a migration-time
  backup as a side effect, but ongoing backup policy is the operator's job
  (Stalwart's own guidance: import/export is explicitly not a backup
  substitute; `Vandelay` per-account export is the documented backup tool).
- Not a cross-major-version skip tool. If the source is older than 0.15.x,
  the tool requires stepping to 0.15.x first (this matches Stalwart's own
  stated constraint — see UPGRADING notes).
- No support for editing mail content during migration (no format
  conversion beyond what Stalwart's own store migration does).

## 3. High-level flow

```
 ┌─────────────┐   ┌───────────┐   ┌────────────┐   ┌───────────────┐   ┌────────────┐   ┌────────────┐
 │  PREFLIGHT  │──▶│  BACKUP   │──▶│ STAGE NEW  │──▶│ RECOVERY-MODE │──▶│  CUTOVER   │──▶│  VALIDATE  │
 │  (checks,   │   │ (defense  │   │  BINARY +  │   │   MIGRATE     │   │ (swap, up, │   │ (functional│
 │  dry-run)   │   │ in depth) │   │  config    │   │ (apply plan)  │   │  smoke)    │   │  + counts) │
 └─────────────┘   └───────────┘   └────────────┘   └───────────────┘   └────────────┘   └────────────┘
        │                 │                │                 │                 │                │
        └─────────────────┴────────────────┴─── on failure ──┴─────────────────┴──▶  ROLLBACK
```

Each box is a **phase**; each phase is a sequence of idempotent, checkpointed
**steps**. State is persisted to disk after every step (§5), so the whole
pipeline can be killed and re-invoked safely.

For a pure patch bump within 0.16.x (no schema change per Stalwart's
changelog through 0.16.14), the plan collapses to: PREFLIGHT → BACKUP →
STAGE → CUTOVER → VALIDATE, skipping the recovery-mode phase entirely (see
§4.6).

## 4. Phases

### 4.1 Preflight

Read-only. Aborts before touching anything if a hard blocker is found;
warns and asks for confirmation on soft blockers.

- Detect current Stalwart version (`stalwart --version`, or JMAP
  `Core/echo`/session endpoint if remote).
- Refuse to run if current version is outside the tool's supported starting
  range (must be ≥0.15.0; older installs are told to upgrade to 0.15.x
  first, per Stalwart's own guidance).
- Detect topology: systemd unit vs. Docker container vs. Compose, single
  node vs. cluster member count (via config/cluster settings), reachable
  peer nodes.
- **Cluster gate**: refuse to proceed unless every node in the cluster is
  confirmed stopped (mirrors the documented hard requirement — one live
  v0.15 node during migration corrupts the store).
- Detect store backend(s): RocksDB, SQLite, FoundationDB, PostgreSQL,
  MySQL, plus configured blob store (local FS / S3-compatible) and FTS
  backend (native / Elasticsearch).
- Disk space check: require free space ≥ N× current data directory size
  (embedded stores need a full copy for the backup step; default threshold
  configurable, hard-fail below a safety floor).
- Resolve and download the target binary/image, verify checksum/signature
  against the published release.
- Fetch and pin the exact upstream `migrate_v016.py` (or its 0.16-successor
  equivalent) revision, hash it, vendor the hash into the run's checkpoint
  record — we depend on it as an external, versioned dependency, not a
  static local copy that can silently drift from upstream.
- Dry-run the settings dump against the live server (read-only JMAP calls)
  to confirm admin credentials and API reachability before anything
  destructive is scheduled.
- Snapshot pre-migration *facts* used later for validation: account count,
  per-account mailbox message counts (IMAP `STATUS`), domain list, DKIM key
  fingerprints, TLS cert fingerprints, listener port list. Stored alongside
  the checkpoint, not derived after the fact.
- Emit a plain-language plan summary and require explicit confirmation
  (`--yes` to skip interactively, but never by default).

### 4.2 Backup — defense in depth

No single backup mechanism is trusted alone, because the risk profile is
different at each layer:

1. **Filesystem/DB snapshot** (infra-level, fast, whole-store):
   - Embedded (RocksDB/SQLite): stop-the-world `cp -a` of the data
     directory (or LVM/ZFS snapshot if available — preferred, since it
     doesn't require the copy to finish before the next step) to a sibling
     path (`<datadir>.v0155-backup`), never overwriting source.
   - External SQL (Postgres/MySQL): targeted dump of the critical table
     set Stalwart's own guide calls out (`s d r h b g j f u` for Postgres;
     equivalent for MySQL), not a full-instance dump — matches the
     documented, tested restore path and stays fast on large installs.
   - FoundationDB: `fdbbackup` against the configured cluster.
2. **Settings/principals export** (the `migrate_v016.py dump` step): captured
   during preflight *and* re-captured immediately before cutover, so the
   export used for the apply reflects the last-known-good state, not a
   stale preflight snapshot if time has passed.
3. **Per-account content export (Vandelay/JMAP)**: for installations under
   an operator-configurable account-count threshold, take a belt-and-suspenders
   full `vandelay import` (i.e. export-to-file) of every account into
   self-contained per-account SQLite archives. This is independent of
   storage backend and of the in-place migration path entirely — if
   everything else somehow goes wrong, mail content is recoverable via
   Stalwart's own documented import path into a clean instance. Skipped
   above the threshold by default (time cost), but available as
   `--full-content-backup` regardless of size.
4. **Binary preservation**: old binary is moved aside (`stalwart.v0155`),
   never deleted, so rollback doesn't depend on re-downloading anything.

Every backup artifact is checksummed and the checksum recorded in the
checkpoint file. Before moving past this phase, the tool **verifies** the
filesystem backup by opening it read-only with the *old* binary in a
throwaway temp directory and confirming it reports the expected version and
a sane account count — catching a corrupt or partial copy before it's relied
on, not after a failed rollback.

### 4.3 Stage

- Install target binary alongside the old one (never overwrite in place).
- Run `migrate_v016.py convert` against the fresh dump to produce
  `config.json` + `export.json`, applying path rewrites for Docker/volume
  layouts detected in preflight.
- Additionally generate an **apply-plan for the settings Stalwart's script
  does not carry over** — SMTP listeners, routing rules, rate limits, spam
  rules, auth backend config — by diffing the old effective config against
  the new schema and emitting a best-effort JMAP object set for
  `stalwart-cli apply`. This is flagged clearly as best-effort and included
  in the final report for manual review; it's the one part of the
  documented procedure that's explicitly manual today, and silently getting
  it wrong (rather than flagging it) would be worse than not attempting it.
- Stage new systemd unit / Compose file changes without activating them.

### 4.4 Recovery-mode migration

This is the phase most exposed to partial-failure — it drives an external
process (the new Stalwart binary) through an undocumented-duration startup,
then drives a second external process (`stalwart-cli apply`) against it over
HTTP. Both are supervised with explicit timeouts and health polling, not
fire-and-forget:

1. Stop the old service.
2. Start the new binary in the foreground with
   `STALWART_RECOVERY_MODE=1` and a freshly generated one-time
   `STALWART_RECOVERY_ADMIN` credential (random, never the operator's real
   password, never logged).
3. Poll the recovery HTTP endpoint until healthy or a timeout elapses; on
   timeout, capture logs and fail into the rollback path rather than
   hanging indefinitely.
4. Run `stalwart-cli apply --file export.json`, then the generated
   best-effort settings plan from §4.3, capturing full output.
5. Verify the apply reported success for every object (the tool parses the
   apply-tool's structured output rather than trusting exit code alone —
   partial application with a zero exit code is exactly the kind of silent
   failure this tool exists to catch).
6. Stop recovery mode cleanly (SIGTERM, not SIGKILL, to let it flush).

Checkpointed after each numbered step, so a crash between "apply succeeded"
and "recovery mode stopped" resumes at step 6 instead of re-running apply
against an already-migrated store.

### 4.5 Cutover

- Update the real systemd unit / Compose config to point at the new binary
  and config, without the recovery env vars (leaving
  `STALWART_RECOVERY_MODE=1` set is a documented footgun — it would recovery-
  boot on every restart).
- Start the service normally.
- Wait for healthy JMAP session response.
- Trigger disk-quota (and tenant-quota, if multi-tenant) recalculation via
  the management API, and poll the task queue until it completes rather
  than firing and moving on.

### 4.6 Patch-bump fast path

For an already-0.16.x install moving to a newer 0.16.x patch (the common
case after the initial major migration, and per the changelog the case for
every release from 0.16.1 through 0.16.14 so far): preflight confirms no
schema-migration flag is set for the target version, and the plan skips
§4.4 entirely — binary swap, restart, same validation suite as §4.7. This
is intentionally the same engine with a shorter plan, not a separate
code path, so it doesn't rot independently.

### 4.7 Post-migration validation

Runs automatically after cutover; failure here triggers rollback (§4.8)
unless `--no-auto-rollback` was passed, in which case it just reports and
exits non-zero.

- **Version check**: reported server version matches the target exactly.
- **Auth check**: WebUI login succeeds over the *configured hostname* via
  HTTPS (not bare IP) — this directly targets the real-world post-0.16
  login failure mode found in the field.
- **Protocol reachability**: JMAP session, IMAP, SMTP (submission + MTA),
  POP3, ManageSieve, CalDAV/CardDAV endpoints all accept a handshake on
  their configured ports.
- **Directory integrity**: account/domain/group counts match the preflight
  snapshot exactly (accounting for the bare-username → email-address
  rewrite, which the tool resolves by comparing normalized identities, not
  raw strings).
- **Content integrity — the core no-data-loss check**: per-account IMAP
  `STATUS (MESSAGES)` compared against the preflight snapshot for every
  mailbox of every account (or a statistically sampled subset above a
  configurable account-count threshold, with the full sweep always
  available via `--full-validation`). Any mismatch is a hard failure.
- **DKIM/TLS check**: key fingerprints and cert validity match or are
  intentionally rotated (new-key generation is an expected v0.16 behavior,
  not a bug — the check distinguishes "changed as documented" from
  "missing").
- **DNS check**: for domains under Stalwart's automatic DNS management
  (new in 0.16), diff expected vs. actual published records and flag
  drift rather than assume the automation ran correctly.
- **Mail-flow smoke test**: send one real message through SMTP submission
  to a dedicated canary mailbox and confirm it's retrievable via IMAP
  within a timeout — the one end-to-end check that nothing upstream can
  fake.
- **Quota check**: recalculation task (§4.5) completed and reported
  numbers are non-zero/sane where preflight showed non-zero usage.

Output is a single structured report (JSON + human summary): pass/fail per
check, with enough detail to hand to the operator or to a rollback decision.

### 4.8 Rollback

Two triggers: automatic (validation failure, unless disabled) or manual
(`stalwart-migrate rollback <run-id>`, usable any time up to a
"rollback window closed" checkpoint the operator explicitly confirms once
they're satisfied — see §6).

Procedure, checkpoint-resumable like everything else:
1. Stop the new service (or recovery-mode process, if failure happened
   there).
2. Restore the filesystem/DB backup from §4.2 to the original path
   (`<datadir>.v0155-backup` → `<datadir>`), or restore the targeted SQL
   dump for external databases.
3. Restore the old systemd unit / Compose config.
4. Restart the preserved old binary.
5. Re-run a reduced version of the §4.7 validation suite against the
   *restored* instance (version check, protocol reachability, directory
   counts) to confirm rollback actually worked rather than assuming it did.
6. Report clearly that the instance is back on 0.15.5 and the new-version
   artifacts (staged binary, export.json, apply-plan) are preserved
   untouched for a retry after the underlying issue is fixed.

Rollback never deletes anything from the failed attempt — a second forward
attempt reuses the existing backup and dumps rather than re-capturing
(faster retry, and one fewer chance for the retry's own backup step to
fail).

**Status: implemented** (`internal/rollback`, plus `internal/service` for
the systemd/Docker control it needs) for the manual trigger. Departures from
the procedure above, and what it still doesn't cover:

- Only the manual trigger exists. The automatic one fires on validation
  failure during a real cutover, and there is no real cutover to fail yet.
- The procedure gains a step 0 this design didn't call out: the
  backup is re-verified against its manifest *before* the service is
  stopped. Finding a corrupt backup is survivable while the failed
  instance is still up, and unsurvivable once the data directory has been
  moved aside.
- FoundationDB is refused rather than attempted: §4.2's backup step only
  *starts* an `fdbbackup` job, and restoring one means `fdbrestore`
  against a quiesced cluster. Refusing up front beats a rollback that
  reports success without restoring anything.
- Step 3 (restore the old unit/Compose config) is wired but inert: it
  restores a preserved service definition if the run recorded one, and
  nothing records one yet because cutover — the phase that would rewrite
  it — doesn't exist. It reports as an explicit skip, not a silent pass.
- For an external SQL store, step 2 replays the critical-table dump in
  place. Unlike the filesystem path, the current contents are *not*
  preserved first; the plan the command prints says so before it acts.

`stalwart-migrate run` without `--dry-run` still refuses, but the reason
has narrowed: what's missing now is §4.5 cutover itself, not the ability to
undo it.

### 4.9 Dry run

`stalwart-migrate run --dry-run` runs the real migration mechanics against a
disposable sandbox clone of the data, so an operator can get genuine
confidence *before* committing to a real cutover — not a simulation that
skips the fragile parts, the actual recovery-mode migration (§4.4) and a
post-migration boot check, just pointed somewhere disposable:

1. **Preflight** (§4.1) runs for real, read-only, against the live instance.
2. **Backup** (§4.2) runs for real too, with one exception:
   `SkipBinaryPreservation` is set, so the production binary at the real
   install path is never moved aside. Taking a *consistent* filesystem
   snapshot of an embedded store still means the live service should be
   stopped first (the same requirement Stalwart's own export tooling has) —
   this tool doesn't automate that stop/start today (no systemd/Docker
   control exists yet), so a dry-run without a manual stop first is a
   best-effort snapshot of a live, in-use store, and the CLI says so.
3. **Convert**: `migrate_v016.py convert` turns the settings/principals dump
   into `config.json` + `export.json`, using the script's own documented
   `--patch-paths <old>=<new>` flag to point the generated config at the
   sandbox data directory instead of the real one. This is the officially
   documented mechanism for exactly this kind of path redirection — the tool
   deliberately does not try to rewrite `config.json`'s contents itself,
   since depending on its exact schema (which has already changed once,
   0.15 → 0.16) is a correctness risk this tool avoids wherever an official
   alternative exists.
4. The verified backup copy is cloned again into the sandbox directory
   (never reusing the same directory recovery mode is about to mutate as the
   one rollback would restore from).
5. **Recovery-mode migration** (§4.4) runs for real against the sandbox:
   the actual target binary, actual `STALWART_RECOVERY_MODE=1` boot, actual
   `stalwart-cli apply`.
6. **Boot check + content integrity**: the migrated sandbox is started once
   more as an ordinary boot (no recovery-mode env vars) and polled until its
   HTTP listener answers, confirming the migrated store doesn't just accept
   a settings apply but actually comes up cleanly afterward. If preflight
   captured a pre-migration snapshot (§4.1, requires `--admin-url`), the
   same boot is then used to capture a fresh post-migration snapshot and
   compare the two — this is the actual no-data-loss guarantee, not just
   "the mechanics ran": every account and mailbox from before must still be
   found afterward (matching by exact name, falling back to the part before
   `@` since v0.16's own migration rewrites bare usernames to full email
   addresses) with an identical message count. A mismatch or a missing
   account fails the check. This covers the message-count half of §4.7's
   full suite; DKIM/TLS fingerprint checks and a live mail-flow SMTP→IMAP
   smoke test are still open.
7. Every byte written by steps 2–6 (the fs-backup copy, settings/principals
   dumps, downloaded `migrate_v016.py`, sandbox clone, and generated
   `config.json`/`export.json`) lives under one per-run directory
   (`work-dir/<run-id>`), which is removed on *every* exit path - success,
   a failed check partway through, or an early refusal - via a deferred
   cleanup, not just the happy path. The only thing left behind afterward
   is the checkpoint's `state.json` under `--state-dir`: a small structured
   success/failure log (which check failed and why), not bulk data.
   `--keep-artifacts` opts out for inspecting a failure. Nothing at the real
   binary path, the real service, or the real data directory's *contents*
   is ever mutated by steps 2–6 in the first place.

A same-boundary patch bump (§4.6) has no recovery phase to simulate — dry
run for that plan is just preflight + backup.

## 5. State machine / checkpointing

Every run gets a `run-id` and a checkpoint file
(`/var/lib/stalwart-migrator/runs/<run-id>/state.json`) written after each
step completes, containing: run-id, source/target version, current
phase/step, timestamps, artifact paths + checksums, and the preflight
snapshot facts used by validation. Steps are pure functions of
(checkpoint-state → new-state); re-invoking `stalwart-migrate run` with an
in-progress run-id resumes at the first incomplete step. Steps are written
to be safe to re-run if they were interrupted mid-execution (e.g. the
filesystem copy step checks for and resumes/redoes a partial copy rather
than trusting a checkpoint that says "started" as if it meant "done").

This is the same shape as a deployment pipeline's state file, deliberately —
the risk profile (long-running, multi-process, must survive being killed)
is the same problem.

## 6. CLI surface

```
stalwart-migrate preflight   [--config PATH] ...              # read-only, prints the report
stalwart-migrate run         --dry-run [--target-binary PATH] ...  # implemented — see §4.9
                              [--keep-artifacts]
                              (without --dry-run: refused today — see §4.8's status note)
stalwart-migrate status      [run-id]                          # implemented
stalwart-migrate rollback    <run-id> [--yes]                   # implemented — see §4.8
stalwart-migrate confirm     <run-id>                          # not yet implemented
stalwart-migrate report      <run-id>   [--json]                # not yet implemented
```

`run` is the only command that mutates anything on a *successful* path, and
it always starts with preflight. `rollback` mutates too, by design — it's
the one command that stops a running mail server and overwrites a live data
directory — so it prints the plan it resolved from the run's checkpoint and
refuses to act without `--yes`. Once rollback exists, `confirm` will be a separate, explicit step
so backups aren't pruned just because validation passed automatically — the
operator gets a beat to actually use the migrated server before disk space
is reclaimed. Default retention if never confirmed: configurable TTL, warns
loudly, never auto-deletes silently. Flags shown here are the design intent;
run `stalwart-migrate <command> -h` for the actual current flag set.

## 7. Project layout (Go, matches this workspace's other CLI tools)

```
stalwart-migrator/
  cmd/stalwart-migrate/     main.go, preflight.go, run.go, status.go, rollback.go — CLI entry + wiring
  internal/plan/            version-boundary → ordered phase list (§4.6)                 [done]
  internal/checkpoint/      run-id, state.json read/write, resume logic (§5)             [done]
  internal/preflight/       §4.1 checks                                                  [done]
  internal/backup/          §4.2 — fs/db snapshot, settings dump+convert, Vandelay export [done]
  internal/recovery/        §4.4 — recovery-mode process supervision + apply             [done]
  internal/validate/        §4.7 — boot-check + content-integrity done; DKIM/TLS + mail-flow not yet [partial]
  internal/rollback/        §4.8                                                         [not started]
  internal/stalwartapi/     Ping + AccountSnapshot, incl. per-mailbox counts via impersonation (§8) [done]
  internal/config/          tool's own config (paths, thresholds, credentials handling)  [not started]
  docs/                     this file + phase-specific notes as they get built out
```

There's no separate `internal/stage` package: the `convert` half of
`migrate_v016.py` lives in `internal/backup` next to `dump` (same script,
same invocation pattern), and the dry-run sandbox-cloning logic that stands
in for the rest of §4.3 currently lives directly in `cmd/run.go` rather than
its own package, pending a real cutover phase to generalize it against.

`internal/stalwartapi` is deliberately the only thing that speaks JMAP/HTTP
to Stalwart — every other package depends on it, not on `net/http` directly,
so auth handling and retry/backoff live in one place. `internal/service` is
the same idea for the other external surface: it is the only thing that
shells out to `systemctl` or `docker`, so the commands that can take mail
delivery down sit in one auditable file rather than in each phase that
happens to need them. Rollback needs it today and cutover will need exactly
the same operations, which is why it's its own package rather than living
inside `internal/rollback`. `preflight.DeploymentKind` is a type alias for
`service.Kind`, so detection and control can't drift apart.

## 8. Open questions for the next pass

- **Credential handling**: recovery-mode admin password and any stored JMAP
  credentials need a real secrets story (env var pass-through is fine for
  v1, but the checkpoint file must never contain them in plaintext).
- **Cluster orchestration**: §4.1's cluster gate assumes the operator stops
  other nodes manually; a v2 could SSH-coordinate that instead. Out of scope
  for v1.
- **`migrate_v016.py` dependency**: pinning by hash is a start, but the
  script is Stalwart's, not ours — need a policy for what happens when it
  changes upstream (re-vendor + re-test before bumping the pin, never
  silently float to `main`).
- **Best-effort settings apply-plan (§4.3)**: needs real-world testing
  against a variety of existing SMTP/routing/spam configs before it's
  trusted un-reviewed; v1 should probably always require operator sign-off
  on that specific generated plan even with `--yes` set for everything else.
  Not started — dry-run currently only replays what `migrate_v016.py`
  itself converts.
- **Account/mailbox enumeration** (`stalwartapi.Client.AccountSnapshot`):
  **implemented**, including per-mailbox message counts. Account count and
  domains come from `x:Account/query` + `x:Account/get` against Stalwart's
  management API (`/api`, capability `urn:stalwart:jmap`), confirmed
  against `crates/jmap/src/principal/{get,query}.rs` and
  `docs/ref/object/account.md`.
  Per-mailbox counts needed a second research pass, because a superuser's
  own JMAP session does **not** implicitly grant cross-account access —
  confirmed by reading `crates/jmap/src/api/session.rs`: the session's
  `accounts` map is built solely from the authenticated identity's own
  membership/sharing grants, unaffected by any admin flag. The real,
  documented mechanism is Stalwart's `impersonate` permission
  (`docs/auth/authorization/administrator.md`): an account holding it can
  log in *as* another account via the composite Basic-auth username
  `<target>%<impersonator>`, after which standard RFC 8621 `Mailbox/get`
  (property `totalEmails`) works normally against that impersonated
  session's own `apiUrl` (session-discovered per RFC 8620, not the `/api`
  management endpoint — confirmed as a distinct endpoint in
  `docs/ref/object/account.md`). `AccountSnapshot` now does this per
  account it finds; a single account's failure (most likely: `impersonate`
  not granted) is recorded in `Snapshot.MailboxErrors` rather than failing
  the whole snapshot, so one misconfigured account doesn't hide a working
  result for every other one.
  One resolved false alarm worth recording: an initial pass of this same
  research, reading Stalwart's `main` branch source directly, reported
  `x:Account` apparently replaced by `x:Principal`/`x:Quota`. Checking the
  *published* docs site directly (which has no `principal.md`/`quota.md`
  page, and still documents `x:Account` with a working example) showed that
  was an unreleased/in-development refactor in `main`, not the interface
  the current released version actually exposes — a reminder that "read the
  source" and "read what's actually shipped" can disagree, and it's worth
  checking both before changing already-working code on the strength of one.
  Preflight now populates `RunState.PreflightSnapshot.MailboxCounts` when
  `--admin-url` is set, and `validate.BootCheck` now compares it against a
  fresh post-migration snapshot as part of the same boot (§4.9 step 6) —
  proven end-to-end with a live smoke test that deliberately made the
  "after" instance report fewer messages than the "before" snapshot and
  confirmed the dry run failed loudly with the exact before/after counts,
  rather than just trusting that. **Still open**: preflight/validate always
  attempt every account serially with no sampling/threshold, which could be
  slow on a large install — `--full-validation`'s sampling idea from §4.7
  hasn't been built yet for this; and DKIM/TLS fingerprint checks plus a
  live mail-flow SMTP→IMAP smoke test (the rest of §4.7's suite) aren't
  implemented. With this done, recovery, backup, dry-run, and account/
  mailbox snapshotting all work end-to-end, and dry-run's comparison is now
  the closest thing to §4.7's actual no-data-loss guarantee this tool has —
  the remaining major gap is `internal/rollback` and real cutover (below).
- **Rollback: done. Real cutover: still missing.** `internal/rollback` and
  `internal/service` are implemented and tested (§4.8), so `stalwart-migrate
  rollback <run-id>` can now undo a run: it re-verifies the backup, stops the
  service, moves the failed attempt aside without deleting it, restores the
  data directory (re-verifying every restored file against the manifest) or
  replays the SQL dump, reinstalls the preserved binary, restarts, and runs a
  reduced validation suite against the *restored* instance rather than
  assuming it worked. `run` without `--dry-run` still refuses, but now for
  the narrower reason that §4.5 cutover itself isn't built - the thing that
  would swap the binary, rewrite the unit, and switch the service over. Doing
  rollback first was deliberate: this tool should never be able to commit to
  a change it can't undo.
- **`confirm` still has no implementation**, so nothing can set
  `RollbackWindowClosed` - rollback honours the flag and refuses when it's
  set, but only a hand-edited state.json can currently set it. Closing the
  window is the point of no return for the backups this restores from, so it
  should land together with the retention/TTL policy §6 describes, not
  before it.
- **Cutover must preserve the service definition it rewrites**, recording it
  as a `service-unit` artifact; rollback's restore step already reads that
  contract and reports an explicit skip until something writes one.
- **Dry-run's un-stopped backup snapshot** (§4.9 step 2): a dry-run still
  backs up a live, in-use store unless the operator stops it manually first.
  `internal/service` now makes doing this properly possible - dry-run just
  hasn't been wired to offer it yet.

## Sources

Grounded in Stalwart's own documentation and community reports as of
2026-08-19:
- [UPGRADING/v0_16.md](https://github.com/stalwartlabs/stalwart/blob/main/UPGRADING/v0_16.md) — exact migration procedure this tool automates
- [UPGRADING/v0_15.md](https://github.com/stalwartlabs/stalwart/blob/main/UPGRADING/v0_15.md) — prior breaking-change boundary
- [Database Migration docs](https://stalw.art/docs/management/maintenance/migration/)
- [Backup docs](https://stalw.art/docs/migration/import-export/backup/) (Vandelay)
- [Upgrading guide](https://stalw.art/docs/install/upgrade/)
- [v0.16 blog post](https://stalw.art/blog/stalwart-0-16/)
- [Discussion #2892](https://github.com/stalwartlabs/stalwart/discussions/2892) — breaking changes overview
- [Discussion #3004](https://github.com/stalwartlabs/stalwart/discussions/3004) — upgrading Q&A
- [Discussion #3025](https://github.com/stalwartlabs/stalwart/discussions/3025) — real post-migration WebUI login failure
- [CHANGELOG.md](https://github.com/stalwartlabs/stalwart/blob/main/CHANGELOG.md) — confirms 0.16.1–0.16.14 carry no further schema migrations
