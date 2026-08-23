# stalwart-migrator

In-place upgrade tool for Stalwart Mail Server, 0.15.5 → latest: no data
loss, a checkpoint at every step so a failure can be undone, and automated
validation that the server still works afterwards.

Go, standard library only — no external dependencies.

## Status

Partially implemented. Roughly 8,400 lines of tested code across backup,
preflight, checkpointing, validation, recovery and rollback. What's missing
now is the real cutover phase — the step that actually swaps the binary and
switches the live service over.

| Command | State |
|---|---|
| `stalwart-migrate preflight` | **Works** — read-only checks and a migration plan |
| `stalwart-migrate run --dry-run` | **Works** — preflight, real backup, sandboxed trial conversion |
| `stalwart-migrate run` | **Refuses on purpose** — see below |
| `stalwart-migrate status <id>` | **Works** |
| `stalwart-migrate rollback <id>` | **Works** — prints its plan; acts only with `--yes` |
| `stalwart-migrate confirm <id>` | Not implemented |
| `stalwart-migrate report <id>` | Not implemented |

**`run` without `--dry-run` deliberately refuses to proceed.** Rollback and
service control now exist, so the reason has narrowed: what's still missing
is cutover itself (ARCHITECTURE.md §4.5) — installing the new binary,
rewriting the service definition, and starting the migrated instance for
real. `run` stops rather than going partway. That refusal is the correct
behaviour today, not a bug.

Rollback was built before cutover on purpose: this tool should never be able
to commit to a change it can't undo. `stalwart-migrate rollback <run-id>`
resolves what it would do from the run's own checkpoint, prints that plan,
and touches nothing without `--yes`.

Package state:

| Package | Lines | Tests |
|---|---|---|
| `internal/rollback` | 1807 | yes |
| `internal/backup` | 1805 | yes |
| `internal/preflight` | 1329 | yes |
| `internal/validate` | 792 | yes |
| `internal/stalwartapi` | 716 | yes |
| `internal/recovery` | 702 | yes |
| `internal/checkpoint` | 559 | yes |
| `internal/service` | 467 | yes |
| `internal/plan` | 196 | yes |
| `internal/config` | stub | — |

## Why not a shell script

Stalwart's 0.15 → 0.16 boundary is not a drop-in binary swap: settings move,
and the data directory has to be migrated rather than merely copied. The
failure mode that matters is a half-migrated mail store with no way back —
which is why backup verification, checkpointing, and rollback are the design
centre rather than conveniences bolted on afterwards.

[`ARCHITECTURE.md`](ARCHITECTURE.md) covers this in full: §1 on why a thin
wrapper is insufficient, §4 on the migration phases, §5 on the checkpoint
state machine, §6 on the CLI surface, and §8 on what is still open.

## Build and test

```sh
go build ./...
go test ./...
```

Requires Go 1.26 or newer.

## Trying it safely

`preflight` is the sensible starting point — its checks against the Stalwart
installation are read-only:

```sh
sudo go run ./cmd/stalwart-migrate preflight
```

It still needs write access, because it records the run as a checkpoint
before doing anything else:

```
create run: checkpoint: create run directory:
mkdir /var/lib/stalwart-migrator: permission denied
```

That path is `checkpoint.DefaultBaseDir`, a compile-time constant with no
flag or environment override — so preflight needs either root or a
pre-created writable `/var/lib/stalwart-migrator`. (`run` takes `--work-dir`
for its scratch space, but that is a different directory and does not move
the checkpoint store.)

`run --dry-run` performs a **real backup**, which touches the live data
directory — read the caveat the command prints before using it on anything
you care about. Where the plan crosses the 0.15/0.16 boundary it clones that
verified backup into a disposable sandbox and converts the copy, leaving the
original untouched.

## Rolling back

`rollback` is the one command that stops a running mail server and
overwrites a live data directory, so it never acts on its own reading of the
situation without showing you that reading first:

```sh
stalwart-migrate rollback <run-id>          # prints the plan, touches nothing
stalwart-migrate rollback <run-id> --yes    # performs it
```

Everything in the plan — which backup, which manifest, which preserved
binary — comes from that run's own checkpoint, so rolling back days later
doesn't depend on remembering any of it. What the checkpoint deliberately
doesn't store (database credentials for an external SQL backend) is what the
flags are for.

The order matters and is not negotiable: the backup is re-verified against
its manifest **before** the service is stopped, because a corrupt backup is
survivable while the failed instance is still up and unsurvivable once its
data directory has been moved aside. Nothing from the failed attempt is
deleted — the half-migrated data directory and the displaced binary are
moved to `.failed-<run-id>` names. Afterwards a reduced validation suite runs
against the *restored* instance (version, reachability, directory counts)
rather than assuming the restore worked; pass `--admin-url` to get the last
two, which are skipped without it.

Every step is checkpointed, so a rollback interrupted partway — which is
exactly when a machine is most likely to be rebooted out from under it —
resumes where it stopped instead of restarting a destructive sequence from
the top. Re-running a completed rollback is inert.

FoundationDB installs are refused rather than attempted: the backup phase
only *starts* an `fdbbackup` job, and restoring one needs `fdbrestore`
against a quiesced cluster.
