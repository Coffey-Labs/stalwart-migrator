# Contributing to stalwart-migrator

How to build and test the tool, and where its design is written down. For what
the tool does and how to use it, see the [README](README.md).

## Build and test

```sh
go build ./...
go test ./...
```

Requires Go 1.26 or newer. The tool is Go standard library only — no external
dependencies — and nothing third-party is vendored.

To try a command from a checkout without installing it:

```sh
sudo go run ./cmd/stalwart-migrate preflight
```

[docs/rehearsal.md](docs/rehearsal.md) explains why even read-only commands need
write access to the checkpoint directory.

## Why not a shell script

Stalwart's 0.15 → 0.16 boundary is not a drop-in binary swap: settings move,
and the data directory has to be migrated rather than merely copied. The
failure mode that matters is a half-migrated mail store with no way back —
which is why backup verification and checkpointing are the design center
rather than conveniences bolted on afterwards — and why the tool refuses to
cut over until you confirm you have a way back.

## Where the design is written down

[`ARCHITECTURE.md`](ARCHITECTURE.md) covers the design in full: §1 on why a
thin wrapper is insufficient, §4 on the migration phases, §5 on the checkpoint
state machine, §6 on the CLI surface, and §8 on what is still open. It was
written before the implementation and records the reasoning; where it and the
code disagree about current behavior — the flags shown in §6, for example —
the code and `stalwart-migrate <command> -h` are right.

[docs/status.md](docs/status.md) has the state of each command and package, and
what has been tested against real servers.

## Testing against something real

Where a phase runs an external program, its tests drive a fake one. That is
sound for logic and ordering, and it is not evidence about production. Before
trusting a change to a migration path, run it on a clone of a real server — see
[Rehearse on a clone first](docs/rehearsal.md#rehearse-on-a-clone-first).
