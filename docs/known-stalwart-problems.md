# Known Stalwart problems

Things about Stalwart's 0.15 → 0.16 upgrade that bite, and what to do about
each: the two server fixes you must make first, the administrator account,
the settings Stalwart's converter drops without saying so, the certificate on
the mail ports, and the extra recovery boot some store migrations need. For
the short version, see the [README](../README.md).

## Two things you must fix on the server first

Neither is something this tool can do for you, and both stop a migration dead.
`preflight` refuses on both, while the mail server is still running — but they
are worth knowing before you book a maintenance window, because fixing them is
a change to your directory, not a flag.

### Remove or collapse multi-tenancy

v0.16 requires a tenant-scoped account to sit on a domain owned by that same
tenant, for its primary domain and every alias. v0.15 imposed no such rule, so
an install that is perfectly valid today can be unrepresentable in v0.16.

Run `stalwart-migrate tenants` to see who owns what.

- Where a domain has no tenant of its own and only one tenant's accounts use
  it, the conversion repairs it for you.
- Where two tenants genuinely share a domain, nothing can. Resolve it in v0.15
  first: give each tenant its own domains, move the accounts into one tenant,
  or remove the tenants entirely.

### Migrate as a directory account, not the built-in admin

A `[authentication.fallback-admin]` from `config.toml` authenticates perfectly
well right up to the moment the migration finishes, and then stops existing —
v0.16 keeps its configuration in the store, so the block defining it is never
read again. The migration itself still succeeds; what you lose is the ability
to verify it, recalculate quotas, or administer the server afterwards. The
next section has the detail.

## You need a named admin account before you migrate

**A config-file fallback admin will not survive the migration.** If the only
administrator you have is an `[authentication.fallback-admin]` block in
`config.toml` — which is what `stalwart --init` sets up — you will come out of
the migration unable to administer the server.

Create a real account in the directory, with the admin role, and confirm you
can log in as it *before* migrating. Four separate reasons, verified against a
real 0.15.5 → 0.16.14 migration:

1. **v0.16 keeps its configuration in the store, not in a file.** After the
   migration the server is started with a config that is little more than a
   pointer at the data store, so the old `config.toml` — and the
   fallback-admin block inside it — is no longer read at all. That credential
   simply stops existing.
2. **`migrate_v016.py` gives every migrated account the `User` role**,
   whatever it held before. An account that was an administrator in v0.15
   comes out authenticating normally and refused every management operation.
   `rehearse` generates the operation that restores it — but the account has
   to exist in the directory for there to be anything to restore.
3. **The account's local part must be unambiguous.** v0.16 identifies an
   account by local part plus domain, so if `admin@one.example` and
   `admin@two.example` both exist, this tool refuses to restore either role
   rather than risk granting administrator rights to the wrong one. It says so
   rather than guessing; you then grant it by hand.
4. **The role has to survive, not just the account.** `tenant-admin` has no
   v0.16 equivalent and is not restored, so an account whose rights came only
   from it authenticates afterwards and is still refused management
   operations. Preflight cannot check this — it cannot know which roles the
   converter will carry across — so confirm on a clone, or immediately
   afterwards, that the account can still administer the server.

`preflight` refuses to proceed if the account you authenticate with is not in
the directory, so this is caught before anything is touched rather than after
the migration completes.

**The practical check:** make sure you can authenticate to the admin API as a
directory account — not as the fallback admin — that its local part is unique
across your domains, and that it holds admin rights through a role other than
`tenant-admin`.

## Stalwart's own converter silently drops ACME

`migrate_v016.py` consumes every `acme.*` setting and emits nothing for them.
They are **not** reported as unmigrated either, so nothing warns you: the
certificate carries over, the provider that renews it does not, and TLS keeps
working until the certificate expires roughly ninety days later.

Check for `acme.*` in your dump before migrating, and recreate an
`AcmeProvider` afterwards if there was one. `accountKey` is server-set in
v0.16, so the existing ACME account cannot be carried over — the server
registers a new one on first issuance.

This tool does not yet generate that object for you. It should: the
supplemental plan already does the equivalent for listeners.
[ARCHITECTURE.md](../ARCHITECTURE.md) §4.6a lists what else the converter drops.

## A certificate that serves HTTPS may not serve the mail ports

A `Certificate` object carried into v0.16 with the right SAN is picked up by
the HTTP listener on its own. **IMAPS, SMTPS and POP3S are not**: they keep
serving a self-signed certificate until `defaultCertificateId` is set on
`SystemSettings` and the server is restarted.

This is the kind of thing that looks fine from a browser and surfaces as a
mail client complaining days later, so check it as part of your
post-migration verification: connect to 993 or 465 and confirm which
certificate you are handed, not just to 443.

This tool does not set it for you. Like the `AcmeProvider` above, it should,
and the supplemental plan is where it belongs. Reported by
[@kaya-eu](https://github.com/Coffey-Labs/stalwart-migrator/issues/1).

## The store migration may need one more recovery boot

**This one is open, and it is the reason to rehearse on a clone.** The
recovery cycle boots the target version once, replays your settings into it,
and stops. Across three real migrations, two different failures showed up
that one extra recovery-mode boot cured:

- the settings apply failing on its very first object with
  `primaryKeyViolation`, where re-running the identical apply against a fresh
  recovery boot went straight through; or
- the next normal start panicking with *"Upgrading to version 0.16 is a
  multi-step process"*, where booting recovery mode once more, letting it come
  up and stopping it cleanly was enough.

Never both on the same run — whichever appeared, one more recovery boot
before the real start got past it. That panic is Stalwart's own, and it
suggests the store migration is not finished when the single boot exits.

The likely fix is a settle boot after the apply. **It is not in the tool**,
because getting an extra recovery boot wrong is its own hazard — see [Do not
boot recovery mode again
afterwards](recovery.md#do-not-boot-recovery-mode-again-afterwards). If you hit
either failure, the extra boot is a manual step. Reported by
[@kaya-eu](https://github.com/Coffey-Labs/stalwart-migrator/issues/1).
