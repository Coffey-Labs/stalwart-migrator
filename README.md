# stalwart-migrator

In-place upgrade tool for Stalwart Mail Server, 0.15.5 → latest: no data
loss, a checkpoint at every step so an interrupted run resumes instead of
restarting, and automated validation that the server still works afterwards.

**Recovery from a failed migration is your own snapshot or backup — this
tool does not undo a migration.** See [Recovery is your
job](#recovery-is-your-job) before using it on anything you care about.

Go, standard library only — no external dependencies.

## Status

Partially implemented. Roughly 8,800 lines of tested code. Every phase
except staging now exists as a package, including cutover, but nothing wires
them into a production run yet, so `run` still refuses.

| Command | State |
|---|---|
| `stalwart-migrate preflight` | **Works** — read-only checks and a migration plan |
| `stalwart-migrate rehearse` | **Works** — read-only; converts your settings and reports what won't carry over |
| `stalwart-migrate run` | **Refuses on purpose** — see below |
| `stalwart-migrate status <id>` | **Works** |
| `stalwart-migrate report <id>` | Not implemented |

**`run` deliberately refuses to proceed.** Cutover (ARCHITECTURE.md §4.5) is
implemented, but nothing calls it: the staging phase (§4.3) and the pipeline
that would run preflight → backup → stage → recovery-mode → cutover →
validate against real paths don't exist yet. `run` stops rather than going
partway. That refusal is the correct behaviour today, not a bug.

**Start with `rehearse` instead.** It is read-only, needs no maintenance
window, and answers the question that actually shapes a migration plan —
see below.

Package state:

| Package | Lines | Tests |
|---|---|---|
| `internal/backup` | 1806 | yes |
| `internal/preflight` | 1329 | yes |
| `internal/stalwartapi` | 1276 | yes |
| `internal/cutover` | 1186 | yes |
| `internal/validate` | 792 | yes |
| `internal/recovery` | 702 | yes |
| `internal/checkpoint` | 556 | yes |
| `internal/service` | 467 | yes |
| `internal/plan` | 195 | yes |
| `internal/config` | stub | — |

## Why not a shell script

Stalwart's 0.15 → 0.16 boundary is not a drop-in binary swap: settings move,
and the data directory has to be migrated rather than merely copied. The
failure mode that matters is a half-migrated mail store with no way back —
which is why backup verification and checkpointing are the design centre
rather than conveniences bolted on afterwards — and why the tool refuses to
cut over until you confirm you have a way back.

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

`rehearse` is the next step, and unlike everything else here it is worth
running today — see the next section.

## Rehearse before you migrate

```sh
stalwart-migrate rehearse --admin-url https://mail.example.com \
    --admin-user admin --target 0.16.14
```

It runs preflight, dumps your settings and principals, converts them with
Stalwart's own `migrate_v016.py`, and reports **both halves** of the result:
the apply plan of what will carry over, and the worklist of what will not.

It copies no data, clones nothing, starts no server, and never writes to the
store, so it is safe to run against production repeatedly and without a
maintenance window.

It also generates a **supplemental plan** for the part it can rebuild
automatically — currently your network listeners, which is the difference
between a migrated server that answers and one that doesn't — and reports
exactly how much of the worklist that covers (on a test instance: 24 of
3,505 keys, and it says so rather than implying more). Review it, then apply
it after `export.json`:

```sh
stalwart-cli apply --file <state-dir>/runs/<run-id>/supplement.json \
    --url https://mail.example.com
```

Expect the worklist to be long. Measured against a real production instance,
`migrate_v016.py` carried **219 of 12,401 settings — 1.8%**. The rest,
including `server.listener`, has to be rebuilt by hand; until it is, a
migrated instance answers on no ports at all. Both outputs are preserved
under `<state-dir>/runs/<run-id>/` (`export.json` and `unmigrated.txt`) even
though the rest of the scratch directory is cleaned up, because they are the
conclusions.

This replaced an earlier `run --dry-run` that cloned the data directory into
a sandbox and migrated the copy. That proved the store opens, at the cost of
copying it twice — while the half that found every real problem needed no
copy at all. ARCHITECTURE.md §4.9 has the reasoning.

## Recovery is your job

**This tool does not undo a migration.** There is no `rollback` command.
Recovery from a failed migration is your own snapshot or backup, taken by
whatever method you already trust and know how to restore — a ZFS, LVM or
btrfs snapshot, a VM or volume snapshot, or a restorable backup. Choosing
that method, taking it, and verifying you can actually restore from it is
out of scope for this tool: it does not take one, does not check that one
exists, and cannot restore from one.

Cutover refuses to start until you confirm a recovery point exists. That
confirmation is an acknowledgement, not a check — nothing here can verify
your snapshot. Its only purpose is that nobody migrates a production mail
server having never been asked the question.

**Take the snapshot with the service stopped** if you want a clean one. A
snapshot of a running Stalwart is crash-consistent rather than clean; RocksDB
will usually recover from its WAL, but "usually" is doing real work in that
sentence.

### Restoring from a snapshot loses mail delivered since

Reverting to any pre-migration recovery point discards mail delivered
between taking it and restoring it. This is inherent to restoring a point in
time and this tool cannot solve it — plan your migration window with that in
mind, and consider holding inbound mail at a secondary MX for the duration
if the gap matters to you.

### What the tool does to make a manual restore easier

- **The old binary is preserved**, never deleted, next to the new one as
  `<binary>.v<old-version>` — so putting things back doesn't depend on
  re-downloading a specific old release under pressure.
- **The original service definition is preserved** as `<unit>.pre-<run-id>`
  before cutover rewrites it, so you aren't reconstructing a unit file from
  memory.
- **The settings and principals dumps** taken during backup stay on disk.
- **Every artifact path and checksum is in the checkpoint**, and
  `stalwart-migrate status <run-id>` prints exactly which steps completed
  and which failed — which is the first thing you want when deciding what to
  restore.

None of this is a substitute for the snapshot. It's what makes the twenty
minutes after restoring one less unpleasant.

### Why it works this way

An earlier version of this tool implemented rollback itself: it restored the
filesystem backup, verified every restored file against a manifest, replayed
SQL dumps, reinstalled the old binary, and re-validated the result. It was
tested and it looked good. It was removed, because restoring bytes correctly
is not the hard part — it copied contents and permissions but not
*ownership*, so run as root it would have produced a byte-perfect,
checksum-verified, root-owned data directory that Stalwart, running as its
own user, could not open, and it would have reported success. A filesystem
snapshot has no such failure mode, because it never lost the metadata to
begin with. ARCHITECTURE.md §4.8 records the full reasoning.

## License

Copyright (C) 2026 LINUXexpert-org

This program is free software: you can redistribute it and/or modify it
under the terms of the GNU General Public License as published by the Free
Software Foundation, either version 3 of the License, or (at your option)
any later version.

This program is distributed in the hope that it will be useful, but WITHOUT
ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
more details.

You should have received a copy of the GNU General Public License along with
this program. If not, see <https://www.gnu.org/licenses/>.

The full text is in [`LICENSE`](LICENSE). No third-party code is vendored —
this tool is standard library only, and the `migrate_v016.py` it downloads
at runtime is Stalwart's own script, fetched rather than redistributed.
