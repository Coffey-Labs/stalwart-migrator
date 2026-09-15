# stalwart-migrator

In-place upgrade tool for Stalwart Mail Server, 0.15.5 → 0.16: no data loss, a
checkpoint at every step so an interrupted run resumes instead of restarting,
and automated validation that the server still works afterwards. Go, standard
library only.

A companion to [**ihasmail**](https://github.com/Coffey-Labs/ihasmail), a
JMAP-first webmail client for Stalwart. That one is what you read your mail in;
this one gets the server underneath it onto a version that speaks the protocol
it needs.

> [!CAUTION]
> **A migration cannot be undone, and this tool does not undo one.** Recovery
> from a failed migration is your own snapshot or backup, taken beforehand and
> checked. `run` will not start until you confirm you have one. See
> [Recovery](docs/recovery.md).

**Full guide:** [docs.ihasmail.org/install/stalwart-migrator](https://docs.ihasmail.org/install/stalwart-migrator/)
walks through the whole upgrade — what to fix first, rehearsing, running it,
and what to check afterwards.

## Requirements

- Stalwart **0.15.5**, as a systemd service or a single Docker container
- root on the mail server
- `python3`, for Stalwart's own `migrate_v016.py`
- `stalwart-cli` **1.0.2 or later**, a separate download from the server
- an administrator account **in Stalwart's directory** — not the
  `fallback-admin` from `config.toml`, which does not survive the migration
- Go **1.26 or newer**, to build

## Fix these on the server first

Both stop a migration, and `preflight` refuses on both:

1. **Tenants that share a domain.** v0.16 requires an account in a tenant to
   use only that tenant's domains. `stalwart-migrate tenants` shows who owns
   what.
2. **A config-file admin.** Migrate as a directory account with the admin role,
   whose local part is unique and whose rights don't come only from
   `tenant-admin`.

Details: [Known Stalwart problems](docs/known-stalwart-problems.md).

## Build

```sh
git clone https://github.com/Coffey-Labs/stalwart-migrator.git
cd stalwart-migrator
go build -o stalwart-migrate ./cmd/stalwart-migrate
```

## Use

Give the admin password with `--admin-password` or
`STALWART_MIGRATE_ADMIN_PASSWORD`.

```sh
# 1. Read-only checks and a migration plan
sudo ./stalwart-migrate preflight --admin-url https://mail.example.com --admin-user admin@example.com

# 2. Read-only: convert your settings and report what won't carry over
sudo ./stalwart-migrate rehearse --admin-url https://mail.example.com --admin-user admin@example.com

# 3. Rehearse the real migration on a clone of the server (strongly recommended)

# 4. Migrate, once you have a snapshot you have checked you can restore
sudo ./stalwart-migrate run --admin-url https://mail.example.com --admin-user admin@example.com \
    --recovery-point-confirmed --yes

# Afterwards
sudo ./stalwart-migrate status <run-id>     # which steps completed
sudo ./stalwart-migrate report <run-id>     # what validation found
```

A Docker container also needs `--container-path-unproven` and `--target-image`;
see [Docker deployments](docs/docker.md). After the migration, check the
certificate on ports 993 and 465, and recreate your ACME provider — Stalwart's
converter drops it without saying so.

## Status

Every command works: `preflight`, `rehearse`, `run`, `tenants`, `status` and
`report`. It has migrated a production server (0.15.5 → 0.16.19, 8 seconds of
downtime) and, in another operator's hands, three more. Two things are still
open: some store migrations need one more recovery-mode boot, which is a manual
step, and the Docker path has never completed a migration against a real
Stalwart image. Details and field reports: [Status](docs/status.md).

## Documentation

| | |
| --- | --- |
| [Upgrade guide](https://docs.ihasmail.org/install/stalwart-migrator/) | The whole upgrade, step by step, on docs.ihasmail.org |
| [docs/rehearsal.md](docs/rehearsal.md) | Running preflight safely, what `rehearse` reports, rehearsing on a clone |
| [docs/docker.md](docs/docker.md) | Container deployments: flags, what cutover carries and refuses, where the config goes |
| [docs/known-stalwart-problems.md](docs/known-stalwart-problems.md) | Tenants, the admin account, dropped ACME, certificates on mail ports, the extra recovery boot |
| [docs/recovery.md](docs/recovery.md) | Why recovery is your snapshot, what the tool keeps, never booting recovery mode again |
| [docs/status.md](docs/status.md) | Command and package state, validation, field reports |
| [ARCHITECTURE.md](ARCHITECTURE.md) | The design: phases, checkpoints, and the reasoning behind them |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Building, testing, and where the design is written down |

## License

Copyright (C) 2026 Coffey Labs. GPL-3.0-or-later: free software, with no
warranty. The full text is in [`LICENSE`](LICENSE).

No third-party code is vendored — the tool is standard library only, and the
`migrate_v016.py` it downloads at runtime is Stalwart's own script, fetched
rather than redistributed.
