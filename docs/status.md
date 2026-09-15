# Status and field reports

What works today, what `run` does, what has been tested against real servers,
and the state of each package. For the short version, see the
[README](../README.md).

## Commands

| Command | State |
|---|---|
| `stalwart-migrate preflight` | **Works** — read-only checks and a migration plan |
| `stalwart-migrate rehearse` | **Works** — read-only; converts your settings and reports what won't carry over |
| `stalwart-migrate run` | **Works** — performs the migration; `--recovery-point-confirmed --yes`. Container deployments additionally need `--container-path-unproven` (see [Docker deployments](docker.md)) |
| `stalwart-migrate tenants` | **Works** — read-only; who owns which domain, and what would block a migration |
| `stalwart-migrate status <id>` | **Works** |
| `stalwart-migrate report <id>` | **Works** — prints what validation found for a run |

Run `stalwart-migrate <command> -h` for the current flags of each.

## What `run` does

`run` performs the migration in this order:

preflight → stage → dump → stop → convert → recovery-mode → cutover → validate

It needs two flags: `--yes` (intent) and `--recovery-point-confirmed` (a claim
that you have a snapshot or backup you have verified you can restore — this
tool cannot undo a migration and will not start without it). Start with
`rehearse` first: it is read-only, needs no maintenance window, and tells you
what `run` will and won't carry over. See [rehearsal.md](rehearsal.md).

An interrupted run resumes from its last completed step with
`stalwart-migrate run --resume <run-id>` and the same flags; `status` lists run
IDs and shows which steps completed.

### Validation after cutover

After cutover, `run` compares the migrated instance against the snapshot
preflight took, and fails the command if an account that existed before is
missing from it. A domain that no longer appears is reported as a warning
rather than a failure: the two versions do not agree on what counts as a
domain — principals on one side, `Domain` objects on the other — and failing a
migration over that difference would abort runs that lost nothing.

The service is left running either way. By that point the store has been
migrated in place, so stopping it would not undo anything; your recovery point
is the way back (see [recovery.md](recovery.md)). `report <run-id>` prints the
same finding again later. Where preflight had no admin URL to snapshot from,
validation reports itself as skipped rather than passed.

## Field reports

### A production server, 2026-08-25

The tool took a live server — nine domains, six accounts, a 2.4 GB RocksDB
store — from 0.15.5 to 0.16.19 with **8 seconds** of downtime, every phase
green including post-cutover validation, mail flowing before and after. That
run was preceded by a full dress rehearsal on a clone of the same server, which
is the practice this project most recommends copying: see [Rehearse on a
clone first](rehearsal.md#rehearse-on-a-clone-first).

### Three servers, reported in issue #1

[@kaya-eu](https://github.com/Coffey-Labs/stalwart-migrator/issues/1)
reported three successful 0.15.5 → 0.16.19 migrations on three servers: a
testing and a production instance, each 16 domains, 55 accounts and roughly
**221 GB** of real mail, and an arm64 home server of 1.4 GB across 181 folders.

Read that with two qualifications:

- **They ran a commit predating the automated Docker cutover**, so they
  performed the cutover by hand. What those runs exercised is preflight, the
  dumps, the settings conversion and the recovery-mode store migration, not
  the container cutover.
- **They hit things worth knowing about before you follow them:**
  - an arm64 binary that was fetched for the wrong architecture (fixed);
  - a store migration that needed one more recovery-mode boot than the tool
    performs (**not fixed** — see [The store migration may need one more
    recovery
    boot](known-stalwart-problems.md#the-store-migration-may-need-one-more-recovery-boot));
  - data loss from booting recovery mode again *after* a completed migration
    (see [Do not boot recovery mode
    again](recovery.md#do-not-boot-recovery-mode-again-afterwards)).

## Code

Roughly 14,600 lines of Go, standard library only, of which about 6,300 are
tests. Every phase exists as a package, and `run` wires them into the
migration described above.

Lines are implementation only; each package carries its tests alongside.

| Package | Lines | Tests |
|---|---|---|
| `internal/stalwartapi` | 1456 | yes |
| `internal/backup` | 1333 | yes |
| `internal/preflight` | 1035 | yes |
| `internal/applyplan` | 913 | yes |
| `internal/cutover` | 784 | yes |
| `internal/recovery` | 431 | yes |
| `internal/checkpoint` | 406 | yes |
| `internal/validate` | 382 | yes |
| `internal/stage` | 233 | yes |
| `internal/service` | 201 | yes |
| `internal/plan` | 130 | yes |
| `internal/config` | stub | — |
