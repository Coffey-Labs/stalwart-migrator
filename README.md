# stalwart-migrator

In-place upgrade tool for Stalwart Mail Server: 0.15.5 → latest, with no
data loss, checkpointed rollback at every step, and automated post-migration
validation.

Design status: architected, not yet implemented — see
[ARCHITECTURE.md](ARCHITECTURE.md) for the full design, the phase-by-phase
plan, and the research it's grounded in.

```
stalwart-migrate preflight   # read-only checks + plan
stalwart-migrate run         # execute the migration
stalwart-migrate status <id>
stalwart-migrate rollback <id>
stalwart-migrate confirm <id>
stalwart-migrate report <id>
```
