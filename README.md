# stalwart-migrator

In-place upgrade tool for Stalwart Mail Server, 0.15.5 → latest: no data
loss, a checkpoint at every step so a failure can be undone, and automated
validation that the server still works afterwards.

Go, standard library only — no external dependencies.

## Status

Partially implemented. Roughly 6,000 lines of tested code across backup,
preflight, checkpointing, validation and recovery; two packages are still
stubs, and that gap is what gates the rest.

| Command | State |
|---|---|
| `stalwart-migrate preflight` | **Works** — read-only checks and a migration plan |
| `stalwart-migrate run --dry-run` | **Works** — preflight, real backup, sandboxed trial conversion |
| `stalwart-migrate run` | **Refuses on purpose** — see below |
| `stalwart-migrate status <id>` | **Works** |
| `stalwart-migrate rollback <id>` | Not implemented |
| `stalwart-migrate confirm <id>` | Not implemented |
| `stalwart-migrate report <id>` | Not implemented |

**`run` without `--dry-run` deliberately refuses to proceed.** A real cutover
needs `internal/rollback` — currently a `doc.go` and nothing else — plus real
systemd/Docker service control. Committing to a migration with no working
rollback would break the single guarantee the tool exists to make, so it
stops rather than going partway. That refusal is the correct behaviour today,
not a bug.

Package state:

| Package | Lines | Tests |
|---|---|---|
| `internal/backup` | 1805 | yes |
| `internal/preflight` | 1324 | yes |
| `internal/validate` | 792 | yes |
| `internal/stalwartapi` | 716 | yes |
| `internal/recovery` | 702 | yes |
| `internal/checkpoint` | 559 | yes |
| `internal/plan` | 196 | yes |
| `internal/rollback` | stub | — |
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
