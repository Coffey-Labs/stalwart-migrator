# Recovery

What happens if a migration fails: why recovery is your snapshot and not this
tool, what a restore costs, what the tool keeps to make a manual restore
easier, and the one thing never to do on a migrated server. For the short
version, see the [README](../README.md).

## Recovery is your job

**This tool does not undo a migration.** There is no `rollback` command.
Recovery from a failed migration is your own snapshot or backup, taken by
whatever method you already trust and know how to restore — a ZFS, LVM or
btrfs snapshot, a VM or volume snapshot, or a restorable backup. Choosing that
method, taking it, and verifying you can actually restore from it is out of
scope for this tool: it does not take one, does not check that one exists, and
cannot restore from one.

Cutover refuses to start until you confirm a recovery point exists, with
`--recovery-point-confirmed`. That confirmation is an acknowledgement, not a
check — nothing here can verify your snapshot. Its only purpose is that nobody
migrates a production mail server having never been asked the question.

A failed run stops and reports; a person decides what to restore.

**Take the snapshot with the service stopped** if you want a clean one. A
snapshot of a running Stalwart is crash-consistent rather than clean; RocksDB
will usually recover from its WAL, but "usually" is doing real work in that
sentence.

## Restoring from a snapshot loses mail delivered since

Reverting to any pre-migration recovery point discards mail delivered between
taking it and restoring it. This is inherent to restoring a point in time and
this tool cannot solve it — plan your migration window with that in mind, and
consider holding inbound mail at a secondary MX for the duration if the gap
matters to you.

## What the tool keeps to make a manual restore easier

- **The old binary is preserved**, never deleted, next to the new one as
  `<binary>.v<old-version>` — so putting things back doesn't depend on
  re-downloading a specific old release under pressure.
- **The original service definition is preserved** as `<unit>.pre-<run-id>`
  before cutover rewrites it, so you aren't reconstructing a unit file from
  memory. For a container, the old container is renamed rather than removed
  and its `docker inspect` is kept; see [docker.md](docker.md).
- **The settings and principals dumps, the apply plan and its supplement** are
  kept in `<state-dir>/<run-id>` — `/var/lib/stalwart-migrator/runs/<run-id>`
  unless you moved it. These four are the only files in a run that cannot be
  produced again afterwards: the dumps need a live pre-migration instance, and
  the plan is what was actually replayed into your store. They are kept
  whether or not the run succeeded and whether or not you passed
  `--keep-artifacts`.
- **Every artifact path and checksum is in the checkpoint**, and
  `stalwart-migrate status <run-id>` prints exactly which steps completed and
  which failed — which is the first thing you want when deciding what to
  restore.

None of this is a substitute for the snapshot. It's what makes the twenty
minutes after restoring one less unpleasant.

## Do not boot recovery mode again afterwards

The migration works by starting the new version once in recovery mode,
replaying your settings into it, and stopping it. That is a one-time step in a
migration, and it is not a general-purpose maintenance mode.

An operator who booted recovery mode again — the same way the migration does,
`STALWART_RECOVERY_MODE=1` against the same data directory — for reasons
unrelated to the migration, on a server that had migrated successfully days
earlier, found that `Domain` and `Account` queries came back empty on the next
normal start. This happened twice, on two different servers. It was not a
stale read: creating a domain that had certainly existed a moment earlier
succeeded, with no `primaryKeyViolation`, so the records were genuinely gone.
Disk usage did not change.

What recovered it both times was re-applying that run's `export.json` and
`supplement.json` against a fresh recovery boot, which is why those two files
are kept for you. If you need to change something after a migration, use the
admin API or `stalwart-cli` against the running server.

This is Stalwart's behavior rather than this tool's, and it is recorded here
because this tool is where you learned the technique. Reported by
[@kaya-eu](https://github.com/Coffey-Labs/stalwart-migrator/issues/1).

This is different from the one extra recovery boot some migrations need
*during* the migration, before the first normal start; see [known Stalwart
problems](known-stalwart-problems.md#the-store-migration-may-need-one-more-recovery-boot).

## Why it works this way

An earlier version of this tool implemented rollback itself: it restored the
filesystem backup, verified every restored file against a manifest, replayed
SQL dumps, reinstalled the old binary, and re-validated the result. It was
tested and it looked good.

It was removed, because restoring bytes correctly is not the hard part. It
copied contents and permissions but not *ownership*, so run as root it would
have produced a byte-perfect, checksum-verified, root-owned data directory
that Stalwart, running as its own user, could not open — and it would have
reported success. A filesystem snapshot has no such failure mode, because it
never lost the metadata to begin with. [ARCHITECTURE.md](../ARCHITECTURE.md)
§4.8 records the full reasoning.
