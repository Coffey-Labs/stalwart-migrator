# stalwart-migrator — Architecture

Status: design, no implementation yet.
Scope: upgrade a Stalwart Mail Server in place from **0.15.5** to the current
latest release (**0.16.14** as of 2026-08-19) with no data loss, a working
a recovery point the operator provides, and an automated post-migration
validation pass.

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
- Nothing destructive happens until the operator has confirmed a recovery
  point exists. This tool does not implement the undo (see the non-goals
  and §4.8); it refuses to start without being told one is in place.
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
- **Not a recovery tool.** Restoring a failed migration is the operator's
  own snapshot or backup, by whatever method they already trust — ZFS/LVM/
  btrfs snapshots, VM or volume snapshots, or a restorable backup. This
  tool does not take one, verify one, or restore from one. §4.8 explains
  why that turned out to be the right split.
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
 │  rehearse)  │   │ in depth) │   │  config    │   │ (apply plan)  │   │  smoke)    │   │  + counts) │
 └─────────────┘   └───────────┘   └────────────┘   └───────────────┘   └────────────┘   └────────────┘
        │                 │                │                 │                 │                │
        └─────────────────┴────────────────┴─── on failure ──┴─────────────────┴──▶  STOP + REPORT
                                                                                     (operator restores
                                                                                      their own snapshot)
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
   never deleted, so putting the machine back by hand doesn't depend on
   re-downloading a specific old release under pressure.

Every backup artifact is checksummed and the checksum recorded in the
checkpoint file. Before moving past this phase, the tool **verifies** the
filesystem backup by opening it read-only with the *old* binary in a
throwaway temp directory and confirming it reports the expected version and
a sane account count — catching a corrupt or partial copy while the
pre-migration instance is still up, rather than after it isn't.

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
  in the final report for manual review; silently getting it wrong (rather
  than flagging it) would be worse than not attempting it.

  **This is no longer an optional enhancement.** Measured against a real
  production instance, `migrate_v016.py` migrated 219 of 12,401 settings —
  1.8% — leaving 12,182 for the operator to recreate by hand, including
  `server.listener`. A migrated instance therefore serves nothing until
  somebody rebuilds its listeners, whatever else went right.

  **Status: started** (`internal/applyplan`), generating `NetworkListener`
  objects from `server.listener.*` and reporting its own coverage. Listeners
  came first because every other unmigrated setting degrades the server
  while this one stops it being a server at all.

  Two rules the package holds to:

  - **Only mappings confirmed against a real v0.16 binary go in.** The
    published schema reference gives `NetworkListener.bind` as a JSON
    array; 0.16.14 rejects that. The encoding it accepts — a value-keyed
    set, `{"[::]:25": true}` — was found by applying a plan to a live
    recovery-mode instance and reading it back with `stalwart-cli
    snapshot`. An unverified guess here produces a plan that fails at apply
    time or, worse, quietly configures the wrong thing.
  - **Coverage is reported, never implied.** Against the smoke instance the
    generator covers 24 of 3,505 unmigrated keys and says "0.7%", listing
    the largest groups it did not touch. A plan that covered a fraction
    while implying completeness would be worse than no plan.

  Operations are emitted as `upsert` with `matchOn: ["name"]`, so a plan
  can be re-run — an operator will run it more than once — and the
  supplement is applied *after* `export.json` rather than merged into it,
  so a generated mapping can never override one the official script got
  right.
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
   timeout, capture logs and stop rather than hanging indefinitely.
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

**Status: implemented** (`internal/cutover`), but nothing calls it yet — see
§8. Notes on how it turned out:

- It refuses to run at all unless the operator has confirmed a recovery
  point exists (§4.8). That's an acknowledgement, not a check — this tool
  can't verify someone else's snapshot — but it makes the irreversibility
  of this phase impossible to walk into unasked.
- The unit is rewritten in place, not generated from a template: an
  operator's unit carries hardening options, limits and dependencies this
  tool has no business having an opinion about, and regenerating it would
  silently drop them. It repoints `ExecStart` (preserving systemd's `-@:+!`
  prefix characters and every argument after the executable), updates
  `--config` if asked, and strips recovery-mode `Environment=` lines. It
  refuses on a unit with no `ExecStart`, and on an `Environment=` line that
  mixes a recovery variable with others — a line it only partly understands
  is one it must not edit.
- The original unit is preserved and recorded as the `service-unit`
  artifact *before* the rewrite, so an operator restoring by hand isn't
  reconstructing a unit file from memory.
- Docker deployments recreate rather than rewrite, because a container
  cannot be edited in place the way a unit file can. That makes silent loss
  the default failure: a container rebuilt without its capabilities, its
  custom network or its device mappings starts cleanly and is quietly not
  the server it was. So the same rule the unit rewrite follows applies
  here - a definition this only partly understands is one it must not
  rebuild - and cutover refuses a container using anything outside the set
  it carries across, naming what it found. The list is conservative and
  deliberately not exhaustive; docker's HostConfig has far more fields than
  it checks, and one it does not know about is a reason not to be
  recreating that container at all.
- The old container is renamed rather than removed, and the old image is
  never pruned. Together they are the container's manual restore path, the
  nearest equivalent to the preserved binary of §4.2: one command starts
  the previous container again. The `docker inspect` of the container as it
  was is preserved as the `container-definition` artifact before anything
  is replaced, for the same reason the unit file is.
- **Status: the container path is implemented and not yet reachable from
  the CLI** - `run` does not pass container options, so a container is
  still refused there. Wiring it up, and lifting preflight's refusal for
  the containers it can now handle, is the remaining work in
  [#3](https://github.com/LINUXexpert-org/stalwart-migrator/issues/3).
- Quota recalculation is the one step allowed to fail without failing the
  phase. Stale counters are an accounting problem; a failed cutover is one
  an operator has to respond to by restoring a machine that is otherwise
  migrated and serving mail correctly. Calling for that over a counter
  would be the worse outcome, so it warns and points at the WebUI's Tasks
  panel.

The quota call itself is grounded in Stalwart's `x:Task` schema reference
(`docs/ref/object/task/`), not guessed: `x:Task/set` creating one
`AccountMaintenance` variant per account with `maintenanceType:
"recalculateQuota"`, exactly as the WebUI's own "Recalculate disk quotas"
fans out. The upgrade guide only documents the WebUI path, so two details
remain unconfirmed against a live server and are called out in
`internal/stalwartapi/task.go`: whether the schema's "read-only" annotation
on `accountId`/`maintenanceType` means "immutable after creation" (it has
to, or the variant couldn't be created), and whether a finished task simply
leaves the queue (`TaskStatus` documents Pending/Retry/Failed with no
success state). That uncertainty is the reason this step warns rather than
fails.

### 4.6 Patch-bump fast path

For an already-0.16.x install moving to a newer 0.16.x patch (the common
case after the initial major migration, and per the changelog the case for
every release from 0.16.1 through 0.16.14 so far): preflight confirms no
schema-migration flag is set for the target version, and the plan skips
§4.4 entirely — binary swap, restart, same validation suite as §4.7. This
is intentionally the same engine with a shorter plan, not a separate
code path, so it doesn't rot independently.

### 4.6a What the converter drops without saying so

`migrate_v016.py` consumes every `acme.*` setting and emits nothing for it,
and does not list those keys as unmigrated either. The effect is a migration
that reports complete success while quietly removing certificate renewal: the
certificate itself carries over, so nothing looks wrong until it expires
about ninety days later. Observed on a production migration, 2026-08-25.

The supplemental plan already generates what the converter leaves behind for
listeners (§4.6). An `AcmeProvider` generator belongs alongside it. The
object shape is known-good, having been applied to a live 0.16.19 server:
`challengeType` and `renewBefore` are enums (`TlsAlpn01`, `R23`), `contact`
is a value-keyed set rather than a list, and `accountKey`/`accountUri` are
server-set and must be omitted — the server registers a fresh ACME account,
since the v0.15 account key cannot be carried across.

### 4.7 Post-migration validation

**What this suite can assert depends on the boundary being crossed, and on
the 0.15/0.16 boundary it is less than this section originally claimed.**
Stalwart 0.15.x reports no per-mailbox message counts at any endpoint, and
the impersonation login 0.16 offers returns 401 there, so there are no
"before" counts to compare against — the before/after message-count
comparison is simply unavailable for the migration this tool exists to
perform. Both versions report per-account used quota, which is captured on
both sides, but §4.5 notes the migration resets quotas to zero pending
recalculation, so it is recorded rather than asserted on. What remains
checkable across the boundary is that every account and every domain
survived, and the reports say so in those words rather than implying a
no-data-loss guarantee that was not measured. See
`internal/validate/content_integrity.go`.


Runs automatically after cutover, against the service cutover has just
started: that is the instance people will actually use — its real config,
its real ports, under its real service manager — and checking it costs no
extra downtime, where booting a second copy inside the maintenance window
would. Failure stops the run, reports loudly, and exits non-zero, leaving
the operator to decide what to restore (§4.8). The service is deliberately
left running: by this point the store has been migrated in place, so
stopping it undoes nothing.

A check that could not be performed is reported as **skipped**, never as a
pass. Preflight only captures the "before" snapshot when it has an admin URL
to capture it from, and a run without one has to say it compared nothing
rather than imply everything survived.

`internal/validate.BootCheck` remains the equivalent for an instance the
tool boots itself, which is what `rehearse` needs; `run` uses `RunLive`.

Of the checks listed below, what exists today is the account/domain
comparison. The rest are the intended shape of the suite, not a description
of it.

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
check, with enough detail to hand to the operator deciding whether to
restore.

### 4.8 Recovery from a failed migration — out of scope

**This tool does not undo a migration.** Recovery is the operator's own
snapshot or backup, taken by whatever method they already trust and know
how to restore: ZFS/LVM/btrfs snapshots, a VM or volume snapshot, or a
restorable backup. This tool does not take one, verify one, or restore from
one. Cutover refuses to start until the operator confirms one exists (§4.5).

This replaced a working, tested rollback implementation, and the reasoning
is worth recording because the deleted code looked good:

- **Restoring bytes correctly is not the hard part; restoring everything
  else is.** The implementation copied file contents and permissions and
  verified every restored file against a manifest — and did not preserve
  ownership. Run as root, it produced a byte-perfect, checksum-verified,
  root-owned data directory that Stalwart, running as its own user, could
  not open. It would have reported success. A filesystem snapshot has no
  such failure mode, because it never lost the metadata in the first place.
- **The external-database path was worse.** `pg_dump` without `--clean`
  emits `CREATE TABLE` + `COPY`; replaying that into a database whose
  tables still exist fails outright, and `ON_ERROR_STOP=1` — added so a
  half-applied restore couldn't be reported as success — turned that into
  a hard failure. The two SQL paths were asymmetric and only one was
  plausibly correct.
- **It was never exercised against anything real.** Every test drove fake
  `systemctl`, `psql` and `stalwart` binaries. That's sound for logic and
  ordering, and it is not evidence about production.
- **Snapshots are already in the operator's runbook.** They are atomic,
  metadata-preserving, cheap with copy-on-write, and cover the whole system
  — binary, unit file, config, data — rather than the subset one tool
  thought to capture.

What this tool keeps doing, so a manual restore is as easy as possible:

- The old binary is preserved next to the new one (§4.2), never deleted.
- The original service definition is preserved as a `service-unit` artifact
  before cutover rewrites it, so the operator doesn't reconstruct a unit
  file from memory.
- Every artifact path and checksum stays in the checkpoint, and `status
  <run-id>` prints exactly which steps completed and which failed.

**The mail-delivery gap is accepted.** Restoring any pre-migration recovery
point discards mail delivered since it was taken. This was equally true of
the rollback implementation, is inherent to restoring a point in time, and
is not something this tool can solve. Plan the migration window
accordingly.

**Two consequences worth being explicit about.** First, the confirmation
cutover requires is an assertion, not a check — an unverifiable promise is
weaker than a guarantee, and the value is only that nobody migrates a
production mail server having never been asked the question. Second, there
is no longer an automatic response to a failed migration: a failure stops
the run and reports, and a human decides what to restore. Both are
deliberate trades for not shipping a recovery path that has never been
tested against a real server.

### 4.9 Rehearsal (was: dry run)

**This section was rewritten after running the previous design against a
real 0.15.5 instance and a real production settings corpus. What it found
inverted the design's assumptions, so the reasoning is recorded here rather
than quietly replaced.**

The original dry run existed to answer *"will the migration mechanics
work?"* — it cloned the data, ran the real recovery-mode migration against
the clone, booted the result, and compared content before and after. Three
findings retire that design:

1. **The mechanics were never the risk.** Backup, settings dump, convert
   and the recovery-mode store migration all worked essentially first time
   against real software. The failures were everywhere else.
2. **The final comparison cannot work, at all.** It needs the migrated
   sandbox to answer an API. `server.listener` is not among the settings
   `migrate_v016.py` migrates, so a migrated instance has no listeners and
   answers on nothing. That is not a sandbox artifact to engineer around —
   it is the true post-migration state.
3. **The expensive half buys the least.** Against a 3.6 GB production store
   the old flow copies the data twice (backup + sandbox clone, ~11 GB and a
   long wait) while reading a live mail store, to prove that RocksDB files
   copy correctly and that recovery mode can open them. Real, but modest.

Meanwhile the cheap half — dump, convert, and report what did *not* convert
— is what caught every problem that would have derailed a real migration:
an empty `defaultHostname` that v0.16 rejects, accounts whose passwords
v0.16 refuses to create, and a reconstruction worklist of 12,182 settings.
It needs no data copy at all.

So the phase reduces to the half that earns its cost:

**`stalwart-migrate rehearse`.** Run preflight (§4.1, read-only), dump
settings and principals from the live instance, run `migrate_v016.py
convert`, and report:

- the generated `export.json` plan (what *will* carry over), and
- `unmigrated.txt` (what will *not*, grouped and counted — see §4.3).

That is the whole phase. It copies no data, clones nothing, starts no
server, and never writes to the store — so it is safe to run against
production repeatedly, early and often, without a maintenance window. It
answers the question that actually decides a migration plan: *what will I
have to rebuild by hand, and does my configuration convert at all?*

The sandbox is gone. Cloning the store to run a migration against the copy
proved only that the store migrates and opens — which is worth something,
but not the disk and the wait, and not the risk of reading a live store to
get it. Where that assurance is wanted, rehearse the whole thing on a
throwaway VM restored from a backup, which is what the smoke environment
already does and does better.

Consequences worth stating, since they make this phase much cheaper than
its predecessor:

- **No target binary is needed.** `convert` is pure Python; nothing in
  this phase executes a Stalwart binary of either version.
- **No disk headroom is needed.** Nothing is copied. Preflight's
  free-space check still runs, but it is anticipating the backup a real
  `run` will take, not anything rehearse does — and it says so.
- **Rehearsal performs no content-integrity comparison.** §4.7 explains
  why that is unavailable on this boundary regardless of how it is staged.

Artifacts live under `work-dir/<run-id>` and are removed on every exit path
unless `--keep-artifacts` is passed — with one deliberate exception.
`unmigrated.txt` is the operator's reconstruction worklist and is preserved
and checksummed even on a clean run, because deleting it would throw away
the most useful output of the whole exercise.

A same-boundary patch bump (§4.6) needs no settings conversion at all, so
`rehearse` for that plan reports that there is nothing to rehearse.

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
stalwart-migrate rehearse    [--keep-artifacts] ...            # read-only; see §4.9
stalwart-migrate run         (refused today — see §8)
stalwart-migrate status      [run-id]                          # implemented
stalwart-migrate report      <run-id>   [--json]                # not yet implemented
```

`run` is the only command that mutates anything: `rehearse` reads the live
instance and writes only inside its own work directory. `run` always starts
with preflight. Nothing in this tool restores a failed migration (§4.8), so there
is no `rollback` command, and no `confirm` step to close a rollback window
that no longer exists.

The migration-time artifacts a run leaves behind — the preserved old binary,
the settings and principals dumps, the preserved service definition, and
and rehearsal's converted plan and unmigrated worklist — are never pruned
automatically.
They're small next to the data directory, they're what a manual restore
reaches for first, and deleting them on a schedule to reclaim disk would be
the tool making a call that isn't its to make. Flags shown here are the
design intent; run `stalwart-migrate <command> -h` for the actual current
flag set.

## 7. Project layout (Go, matches this workspace's other CLI tools)

```
stalwart-migrator/
  cmd/stalwart-migrate/     main.go, preflight.go, run.go, status.go — CLI entry + wiring
  internal/plan/            version-boundary → ordered phase list (§4.6)                 [done]
  internal/checkpoint/      run-id, state.json read/write, resume logic (§5)             [done]
  internal/preflight/       §4.1 checks                                                  [done]
  internal/backup/          §4.2 — fs/db snapshot, settings dump+convert, Vandelay export [done]
  internal/recovery/        §4.4 — recovery-mode process supervision + apply             [done]
  internal/cutover/         §4.5 — binary swap, unit rewrite, restart, quota rebuild    [done, unwired]
  internal/validate/        §4.7 — boot-check + content-integrity done; DKIM/TLS + mail-flow not yet [partial]
  internal/service/         systemd/Docker start+stop, used by §4.5                      [done]
  internal/stalwartapi/     Ping, AccountSnapshot (per-mailbox counts via impersonation), quota tasks [done]
  internal/config/          tool's own config (paths, thresholds, credentials handling)  [not started]
  docs/                     this file + phase-specific notes as they get built out
```

There's no separate `internal/stage` package: the `convert` half of
`migrate_v016.py` lives in `internal/backup` next to `dump` (same script,
same invocation pattern). The sandbox-cloning logic that used to stand in
for the rest of §4.3 lived directly in `cmd/run.go` and goes away with the
sandbox (§4.9); what §4.3 still needs is the apply-plan generator, which
has no code yet at all.

There's no `internal/rollback` either, and that's a deliberate removal
rather than a gap — see §4.8.

`internal/stalwartapi` is deliberately the only thing that speaks JMAP/HTTP
to Stalwart — every other package depends on it, not on `net/http` directly,
so auth handling and retry/backoff live in one place. `internal/service` is
the same idea for the other external surface: it is the only thing that
shells out to `systemctl` or `docker`, so the commands that can take mail
delivery down sit in one auditable file rather than in each phase that
happens to need them. `preflight.DeploymentKind` is a type alias for
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
- **Most of the "unmigrated" settings are not work at all - measured, then
  classified.** The raw figure from a production instance was 12,182
  settings, which reads as an impossible amount of manual reconstruction.
  Snapshotting a migrated v0.16.14 store showed why it is misleading:
  8,547 of them are `server.blocked-ip`, runtime auto-ban state that
  repopulates itself; 3,337 are stock spam-filter and lookup data v0.16
  ships its own copies of (2,084 `MemoryLookupKey`, 66 `SpamRule`, 18
  `SpamDnsblServer` were already present after migration); ~224 had already
  been carried by another route, DKIM included, as `DkimSignature` objects
  with their private keys. That leaves **~293 keys genuinely needing a
  human**.

  `backup.UnmigratedReport.Classify` encodes this, and the rehearsal reports
  the categorised view rather than the raw count. Every rule was checked
  against a migrated instance rather than inferred, and an unrecognized
  prefix defaults to "needs review" - assuming an unknown setting is safe to
  ignore is the wrong default.

  This also retired a planned pair of generators. v0.15's spam rules are
  `stwt_rbl_senderscore_ip`; v0.16's are `STWT_RBL_SENDERSCORE_IP` - the
  same stock set, already installed. Generating them from v0.15 would
  duplicate every rule and revert a year of upstream updates, so the
  lookup and spam-filter generators were deliberately not written. The
  targets worth generating are the small site-specific groups:
  `queue.schedule`, `queue.tls`, `session.auth`, `server.auto-ban`.
- **Settings apply-plan (§4.3): started, and the critical path.**
  `internal/applyplan` covers `server.listener` — verified end to end by
  applying a generated plan to a real 0.16.14 instance and reading back all
  ten listeners with correct protocols, binds and TLS flags. Everything
  else in the worklist is still manual: the largest groups are
  `lookup.url-redirectors`, `lookup.trusted-domains`, `spam-filter.list`,
  `spam-filter.rule` and `spam-filter.dnsbl`, each needing its own
  confirmed mapping (`x:StoreLookup`, `x:HttpLookup`, `x:SpamRule`,
  `x:SpamDnsblServer`). The generated plan should still require explicit
  operator sign-off even with `--yes` set for everything else.
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
  **Superseded in part.** That mailbox-count comparison was verified only
  against fabricated fixtures, and against a real 0.15.5 source it does not
  work at all: 0.15.x reports no per-mailbox counts and refuses the
  impersonation login, so the "before" side is always empty, and the
  comparison used to iterate that empty map and report "all message counts
  match" — a vacuous pass on the strongest claim this tool makes. It now
  states plainly when counts were not compared (§4.7). Account and domain
  enumeration against 0.15.x works via its REST principal API.
  **Still open**: preflight/validate attempt every account serially with no
  sampling, which could be slow on a large install; and DKIM/TLS
  fingerprint checks plus a live mail-flow SMTP→IMAP smoke test (the rest
  of §4.7's suite) aren't implemented.
- **Cutover is built; nothing wires it into a production run yet.**
  `internal/cutover` (§4.5) and `internal/service` are implemented and
  tested against fakes. `run` still refuses, for one remaining reason:
  **§4.3 stage doesn't exist**, and neither does the production pipeline
  that would run preflight → backup → stage → recovery-mode → cutover →
  validate against real paths. What stage still needs: downloading and
  verifying the target binary into a staging path
  (`preflight.ResolveRelease` and `backup.DownloadFile` between them
  already have the pieces), running convert against real paths, and the
  settings apply-plan — which, per the measurement above, is what decides
  whether the migrated server serves anything at all.
- **What has and hasn't been proven against real software.** A smoke VM
  (Debian 13, real Stalwart 0.15.5 under a real systemd unit, RocksDB,
  seeded accounts and mail) has now exercised preflight, backup, the
  settings dump, `migrate_v016.py` convert, and the recovery-mode store
  migration end to end - and a scrubbed copy of a production settings
  corpus has been through the converter. Everything in §4.9's rewrite and
  most of §8's newer entries came from that, not from reading code.

  **Cutover has now run**, in a complete 0.15.5 -> 0.16.14 migration of the
  smoke VM: binary verified and installed, the unit rewritten with its
  hardening intact, service restarted, health check passed, and checkpoint
  resume exercised. All mail survived and was readable afterwards. Three
  defects came out of it and are fixed:

  - The converted config was installed root-owned while the service runs as
    its own user, so it crash-looped 28 times on "Permission denied". The
    ownership trap that retired the rollback implementation (§4.8), in a
    new place. Cutover now installs the config itself, copying ownership
    and mode from the config being replaced.
  - **v0.16.14 does not serve `/api`.** Confirmed against a fully migrated,
    fully configured instance, not just a sandbox: `/api`, `/api/principal`
    and `/jmap/` all 404, and the JMAP endpoint is the one the session
    document advertises. The client now discovers it, re-basing the
    advertised path onto the operator's own host - a real instance
    advertises a canonical public URL that frequently isn't reachable from
    where this tool runs.
  - Dispatching on the `urn:stalwart:jmap` capability was wrong, because
    *neither* version advertises it. The client probes what the instance
    actually serves instead.

  Still unproven: the **`x:Task` quota wire format** (the endpoint fix makes
  it reachable, but the migrated instance refused the call - below),
  **systemd drop-in** handling, and anything on a **non-RocksDB backend**
  or a **Docker** deployment.
- **Rehearsal has been run against a live production instance**, read-only,
  and found two preflight defects a test instance could not have:

  - **Store-backend detection missed a flat config entirely.** A config
    generated by `stalwart --init` declares `type = "rocksdb"` inside a
    `[store.rocksdb]` section; the production config has *no section
    headers at all* and declares `store.rocksdb.type = "rocksdb"` flat.
    Detection checked only for a bare `type` key, so it found nothing and
    left `topology.store_backend` empty - and `backup.Run` treated an
    unrecognized backend as a *skip*, meaning a real run would have
    proceeded with no filesystem backup whatsoever. Both fixed: flat keys
    are detected, and an unrecognized backend is now a hard failure,
    because the one artifact this phase exists to produce must not be
    quietly absent.
  - **The cluster warning didn't say where it matched.** It fires on any
    occurrence of "cluster" anywhere in the config, which is the right bias
    - a missed cluster is the dangerous direction - but on the production
    config the sole match was inside the *value* of an unrelated setting.
    The warning now names the location and says whether the match was the
    setting or only its value, which turns a config-wide search into a
    glance.

  Also confirmed there: the HTTPS path works against a real certificate,
  and the coverage numbers match the earlier scrubbed-corpus measurement
  exactly (12,182 unmigrated). Account enumeration past one page is still
  untested - that instance has six accounts, not the hundred-plus the
  pagination loop exists for.
- **A migrated instance has no working administrator - diagnosed and
  fixed.** `migrate_v016.py` assigns every migrated account the `User`
  role regardless of what it held before, so an account that was an
  administrator in v0.15 comes out authenticating normally and refused
  every management call. Ordinary users are unaffected: `User` is what they
  had and what they get, and their credentials, mail and mailboxes all
  survive untouched.

  The v0.16 shape came from the server's own schema document
  (`GET /api/schema`): `x:UserRoles` is a multi-variant type with variants
  `User`, `Admin` and `Custom`, and `Account` is itself multi-variant, so
  the upsert needs its own `@type` as well. `applyplan.AccountRoleOperations`
  restores it from the principals dump, emitting operations only for
  accounts whose role actually changes - rewriting every account would be a
  far larger blast radius for no benefit. Where v0.15 listed several roles,
  admin wins and the collapse is reported; roles with no v0.16 equivalent
  are named rather than dropped silently.
- **`x:Account.domainId` returns an internal id on v0.16 - resolved.** A
  pre-migration snapshot records domains as names ("smoke.test"); the same
  instance afterwards reported "b", so the §4.7 directory comparison would
  have read every domain as having vanished. The client now resolves ids to
  names with `x:Domain/query` + `x:Domain/get` in a single request via a
  JMAP back-reference, confirmed against a live 0.16.14. An id that cannot
  be resolved is kept as-is rather than dropped, since a domain that can't
  be named is still a domain that exists; a failure of the resolution call
  itself is an error, because silently comparing ids against names is the
  bug this fixes.
- **Quota recalculation is grounded but unproven.** The `x:Task` wire
  format comes from Stalwart's schema reference rather than a live server;
  §4.5 lists exactly which two details are inferred. A smoke test against a
  real 0.16 instance would settle both, and would let this step be promoted
  from "warns on failure" to a hard check.
- **Docker is implemented but not yet wired to the CLI.** Preflight
  inspects a container and reports what stands in the way; stage pulls and
  verifies an image; the recovery cycle runs in a throwaway container
  against the live data; and cutover recreates the container, refusing one
  whose definition it would not carry across intact. What is missing is
  `run` passing those options and preflight lifting its refusal for the
  containers now handled. Compose stays refused deliberately: recreating a
  compose-managed container out from under compose leaves the container and
  the compose file disagreeing, and the next `compose up` reverts the
  migration.
- **Cutover ignores systemd drop-ins.** It rewrites only the main unit
  file, so an `ExecStart` or `Environment` override in
  `/etc/systemd/system/stalwart.service.d/*.conf` is invisible to it -
  including a recovery-mode variable set there, which is exactly the
  footgun the rewrite exists to prevent. Drop-ins are common enough that
  this needs handling before a production run, at minimum by detecting
  them and refusing.
- **Nothing prevents concurrent runs.** Two invocations against the same
  run-id would both proceed; there's no lock file or equivalent.
- **A full dress rehearsal has been run against a clone of production** -
  the real 3.6 GB store, 12,361 settings, 6 accounts across 9 domains,
  streamed into the smoke VM and migrated 0.15.5 -> 0.16.14 with the tool's
  own phases. It succeeded, and the timings are the useful part: the
  recovery-mode conversion of that store took **2 seconds**, and the whole
  sequence from service-stop to service-start was seconds of work. A
  migration window is dominated by waiting and verification, not by data
  volume - worth knowing before scheduling one around store size.

  Four things it found that smaller instances could not:

  - **Account roles broke on production-shaped names.** v0.16 stores an
    account as a local part plus a domain reference; the generator was
    passing v0.15's full address and the server rejected it ("Invalid email
    local part"). The smoke instance used bare usernames and never
    exercised it. Fixed - and because local parts are unique only within a
    domain, an ambiguous one is now refused with a warning rather than
    risking Admin landing on the wrong account.
  - **A failed apply leaves the store in bootstrap mode.** After a partial
    apply the instance answers every management call with "The server is in
    bootstrap mode. Only the 'Bootstrap' object type can be accessed until
    the bootstrap process is complete." So a half-applied plan is not a
    partially configured server, it is an unusable one, which raises the
    stakes on apply failures considerably.
  - **A config fallback-admin does not survive the migration.** v0.16's
    config is a store pointer, so an `[authentication.fallback-admin]`
    block in the old config.toml simply ceases to exist. The credentials an
    operator supplies for the pre-migration instance therefore stop working
    on the migrated one, and cutover's health check - which authenticated -
    failed a cutover that had actually succeeded. Liveness and credentials
    are now separate questions: any response proves the service is up, and
    credentials that no longer work are a warning naming this cause.
  - `tenant-admin` has no v0.16 equivalent and is reported as unrestorable
    rather than silently dropped.

- **A domain and the accounts on it must agree about their tenant in v0.16,
  and Stalwart's converter does not make them agree.** A second live attempt
  on 2026-08-24 got further - preflight clean, binary staged, settings dumped
  and converted - and then failed during recovery-mode migration, again with
  the service already stopped:

      created Tenant (1)
      created Domain (9)
      create Account restore-13: invalidForeignKey | Object id: Domain#d

  In v0.15 a domain's tenant and a principal's tenant were independent facts.
  v0.16 requires a tenant-scoped Account to sit on a Domain owned by that
  same tenant - for its primary domain and for every alias - and answers
  `invalidForeignKey` on the Domain reference otherwise. `migrate_v016.py`
  carries the two facts over independently: `_build_domains` sets a domain's
  `memberTenantId` only when the domain appears as a declared `domain`
  principal carrying a `tenant`, while `_build_user` sets the account's from
  the account's own record. A domain that exists only inside an email address
  is inferred, gets no tenant, and any tenant-scoped account using it is then
  rejected.

  This was established by reproduction, not inference. A synthetic v0.15
  principal dump run through the unpatched converter and applied to a real
  0.16.14 in recovery mode reproduces `invalidForeignKey | Object id:
  Domain#d` character for character - the `#d` is the server's own object id
  for the offending domain, not a plan client-id. The same harness shows
  which directions are actually constrained:

  | account | domain | result |
  | --- | --- | --- |
  | tenant-scoped | no tenant | **rejected** |
  | tenant-scoped | a different tenant | **rejected** |
  | tenant-scoped | its own tenant | accepted |
  | global | tenant-owned | accepted |

  Only the first two fail, and only the first is repairable: where a
  tenant-less domain is used exclusively by accounts of one tenant, giving
  the domain that tenant is the sole assignment that both applies and keeps
  every account. `applyplan.ReconcileDomainTenants` does that to the plan
  between `convert` and `apply`, reports each adoption, and refuses - without
  modifying anything - when the accounts genuinely disagree, since forcing
  such a plan through would mean dropping mailboxes. The same fix has been
  prepared for `migrate_v016.py` upstream; the tool downloads that script
  rather than vendoring it, so the repair lives here until a released version
  carries it, and is a no-op on a plan that is already consistent.

  An earlier version of this section claimed the converter emitted every
  Account with `tenantId: null`. That was wrong: the field is
  `memberTenantId`, the converter does populate it, and the export had been
  inspected for a key no version of the script ever writes. Preflight briefly
  refused every multi-tenant install on the strength of that misreading. The
  refusal is now narrowed to the arrangements v0.16 truly cannot represent.

  The tool's failure was in *when* this was discovered. Tenant membership
  is readable while the server is still running, so preflight now maps it
  (`stalwartapi.Client.FetchTenantLayout`), predicts the outcome with the same
  rule the server enforces, and either warns about the domains that will adopt
  a tenant or fails - before anything is touched. That is the same lesson as
  the stalwart-cli check immediately below: both were knowable in advance, and
  both were found after a production mail server had been stopped. Any future
  dependency of the *conversion* belongs in preflight, not in the phase that
  needs it.

- **A live migration attempt failed and cost a restore. Three defects, all
  fixed, all now proven against a reproduction.** On 2026-08-24 a real
  migration stopped a production mail server and then discovered the host's
  `stalwart-cli` was 0.13.4 - present, but from when the CLI shipped with
  the server, and with no `apply` command. Recovery was closed in both
  directions: v0.16's recovery-mode boot had already bumped the store schema
  to v6 (`expected 5 or below, found 6`), so the old binary could not reopen
  it, and going forward needed `export.json`, which the failure path had
  deleted - regenerating it required a settings dump from a live v0.15
  instance that could no longer start. The operator restored a day-old
  snapshot and lost a day of mail.

  - **Preflight now verifies the external tools** (`CheckExternalTools`),
    before anything is touched: stalwart-cli must exist and be v1.0.2 or
    later, and python3 must run. Every fact needed to prevent that outage
    was available in under a second from a stopped state. Skipped entirely
    for a patch upgrade, which invokes neither tool.
  - **A failed run no longer deletes its own inputs.** The cleanup applied
    to every exit path, which was right for a sandboxed rehearsal and
    catastrophic here: after the service is stopped, the settings dump
    cannot be regenerated, so deleting it removes the only way forward.
  - **`run --resume <id>` continues an interrupted run.** The checkpoint
    machinery existed but never engaged, because `run` created a new run
    every invocation - so a retry would re-run preflight against a binary
    already moved aside and fail. Completed steps are now skipped.

  Verified on a VM built to match: stalwart-cli 0.15.5 installed, accounts
  and mail seeded. Preflight refused with the service still running and mail
  still flowing; a stub CLI that passed the version check and failed the
  apply left the run stopped with all eight inputs intact; and `--resume`
  carried it to a clean finish - five seconds of downtime, quotas rebuilt.
  That failure-path test is the one that should have run before production,
  and did not.

- **`run` is built and works.** preflight -> stage -> dump -> preserve ->
  stop -> convert -> supplement -> recovery-mode -> cutover, checkpointed
  throughout, verified end to end against a real 0.15.5: mail down for six
  seconds, users unchanged, and `recalculate-quotas` succeeding for the
  first time - the `x:Task` wire format inferred from the schema reference
  turned out to be right, once the endpoint discovery and role restoration
  made it reachable at all.

  Two gates, deliberately separate: `--yes` is about intent, and
  `--recovery-point-confirmed` is a claim about the world that this tool
  cannot verify and must not assume. `internal/stage` (§4.3) fetches the
  release, refuses to substitute a different build for the one it wants,
  honours a pinned checksum, and confirms the extracted binary reports the
  version its tag claims - because everything upstream of that check is an
  assumption about somebody else's release process.

- **`rehearse` (§4.9) is designed but not built.** The command is still
  `run --dry-run` with the old sandbox-cloning shape. Building it is mostly
  deletion: the dump, convert and report pieces already exist and work
  against real instances; what goes away is the backup clone, the sandbox,
  the recovery-mode cycle and the boot check. `cmd/run.go`'s sandbox logic
  disappears with it, which also removes the reason §7 gives for there
  being no `internal/stage` package.
- **Post-migration validation has no reachable instance to validate**
  (§4.7/§4.9). Until the apply-plan reconstructs listeners, nothing that
  boots from a converted store can answer an API, so "did the migration
  preserve the data" cannot be asked of the migrated instance at all. This
  is the strongest argument for building the apply-plan first.

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
