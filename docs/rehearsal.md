# Trying it safely, and rehearsing

How to find out what a migration will do before it does it: running
`preflight` safely, what `rehearse` reports, and how to rehearse the whole
migration on a clone of your server. For the short version, see the
[README](../README.md).

## Running preflight

`preflight` is the sensible starting point. Its checks against the Stalwart
installation are read-only:

```sh
sudo go run ./cmd/stalwart-migrate preflight
```

It still needs write access, because it records the run as a checkpoint before
doing anything else. Without it, you see:

```
create run: checkpoint: create run directory:
mkdir /var/lib/stalwart-migrator: permission denied
```

Checkpoints go to `/var/lib/stalwart-migrator/runs` by default
(`checkpoint.DefaultBaseDir`). Either run as root, pre-create that directory
writable, or point `--state-dir` somewhere writable. `run` and `rehearse` also
take `--work-dir` for their scratch space, which is a different directory and
does not move the checkpoint store.

Pass `--admin-url` and `--admin-user` for the reachability check, and the
password with `--admin-password` or `STALWART_MIGRATE_ADMIN_PASSWORD`.

## What rehearse reports

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

### The supplemental plan

It also generates a **supplemental plan** for the part it can rebuild
automatically — currently your network listeners, which is the difference
between a migrated server that answers and one that doesn't — and reports
exactly how much of the worklist that covers (on a test instance: 24 of 3,505
keys, and it says so rather than implying more).

`run` generates the same supplement and applies it after `export.json` for you.
If you are replaying a rehearsal's plan by hand instead, review it, then apply
it after `export.json`:

```sh
stalwart-cli apply --file <state-dir>/<run-id>/supplement.json \
    --url https://mail.example.com
```

It is applied after `export.json` rather than merged into it on purpose: the
official conversion is the authority on everything it handles, and a generated
plan that overlapped it could silently override a correct mapping with a
guessed one.

### Reading the worklist

The worklist is long but mostly not work, and `rehearse` says which is which.
Measured against a real production instance, `migrate_v016.py` carried
**219 of 12,401 settings**. Of the 12,182 it left:

- **8,547** are runtime auto-ban state that repopulates itself
- **3,337** are stock spam-filter and lookup data v0.16 ships its own copies
  of — restoring v0.15's would revert a year of upstream updates
- **~224** were already carried another way, DKIM signatures included
- **~293** genuinely need your eyes

`server.listener` is in that third group only because this tool regenerates it
for you; without that a migrated instance answers on no ports at all.

### What it keeps

The conclusions are preserved under `<state-dir>/<run-id>/` —
`/var/lib/stalwart-migrator/runs/<run-id>/` by default — as `export.json`,
`unmigrated.txt` and `supplement.json`, even though the rest of the scratch
directory is cleaned up (unless you pass `--keep-artifacts`).

### Why it replaced a dry run

`rehearse` replaced an earlier `run --dry-run` that cloned the data directory
into a sandbox and migrated the copy. That proved the store opens, at the cost
of copying it twice — while the half that found every real problem needed no
copy at all. [ARCHITECTURE.md](../ARCHITECTURE.md) §4.9 has the reasoning.

## Rehearse on a clone first

`rehearse` is read-only and stops short of the half that matters: it converts
your settings but never applies them, and applying is where a real migration
fails. A clone closes that gap, and on a 2.4 GB store it costs about six
seconds of production downtime to build.

1. **Copy the data directory with the service stopped.** RocksDB is
   single-writer, so a hot copy may be torn — and a rehearsal on a torn store
   fails for reasons production never would, or passes when it should not.
   Stop, `cp -a` the data dir to the same disk, start again, archive it
   afterwards; the service is down only for the local copy.
2. **Give the guest no route off the host.** A clone of a live mail server
   will otherwise renew certificates for your real domains and deliver
   whatever is in the outbound queue. In libvirt that means a network with no
   `<forward>` element. Verify it from inside the guest rather than assuming.
3. **Run the real thing**: `preflight`, then `run` with `--target-binary`,
   `--stalwart-cli` and `--migration-script` pointed at locally staged copies,
   since an isolated guest can download nothing.
4. **Snapshot the guest while it is shut down**, so a failed attempt costs
   seconds to reset rather than a rebuild.

### What it caught

None of these could be expressed by the mock:

- a multi-tenant arrangement v0.16 cannot represent;
- `--target-binary` never reaching preflight, so an air-gapped host failed on a
  release lookup;
- an `admin` account that was a config fallback-admin and stopped working the
  moment the migration finished;
- `tenant-admin` roles the converter does not restore.

### What isolation costs

Two things the isolation costs, so they are not mistaken for faults: the guest
cannot fetch the v0.16 web interface, so `/account/` returns 404, and
certificate renewal cannot be exercised at all.
