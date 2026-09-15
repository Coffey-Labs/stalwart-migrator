# Docker deployments

How `run` migrates a Stalwart running in a Docker container: the flags it
needs, what it carries across, what it refuses, and what it keeps for a manual
restore. For the short version, see the [README](../README.md).

## The container path is unproven

**The container path has never completed a migration against a real Stalwart
image.** What it inspects and what it assembles have been checked against one,
which is how two problems were found and fixed (#11) — but a fake `docker`
still proves only that the right commands are assembled, not that the image
reads the config it is handed and comes up as the server it was.

`run` refuses a container deployment unless you pass
`--container-path-unproven`, which is there so nobody reaches it without being
told. Rehearse on a clone first ([rehearsal.md](rehearsal.md)); that advice
goes double here.

## Flags

- `--target-image` names the image in full, e.g.
  `stalwartlabs/stalwart:v0.16.14`. It is never derived from the running
  container by swapping the tag — that is wrong for a digest-pinned image, a
  mirror or a fork, and being wrong means pulling the wrong software into a
  mail server.
- `--container` names the container (default `stalwart`).
- `--data-dir` must name the path **inside** the container, since that is
  where its data actually lives. `preflight` says so if it matches none of the
  container's mounts.

## What cutover carries across, and what it refuses

A container cannot be edited in place the way a unit file can, so cutting one
over means rebuilding it. A container rebuilt without its capabilities, its
custom network or its device mappings starts cleanly and is quietly not the
server it was.

So cutover carries across what it understands and refuses outright when it
finds anything else, naming what it found. It asks that question in
`preflight`, while the server is still running, rather than only at cutover
after it has stopped.

**It carries:** mounts, ports, environment, restart policy, labels, and
anything the container overrides on its image — a `--user`, an
`--entrypoint`, a command of your own. What the container merely *inherits*
from its old image is left to the new one, whose own defaults are the ones
that go with it.

**It refuses:**

- a container with settings it doesn't understand, such as extra
  capabilities, a custom network or device mappings;
- a container whose data is not on a volume — an upgrade replaces the
  container, and the writable layer goes with it;
- a container managed by Docker Compose, because recreating it out from under
  compose leaves the container and the compose file disagreeing about what is
  deployed, and the next `compose up` reverts the migration. Compose
  deployments are migrated by editing the image tag in the compose file and
  running `compose up -d`.

## Where the converted config goes

The converted v0.16 config is written into the host side of whichever mount
covers `--data-dir`, named on the container side, and the recreated container
is started with `--config` pointing at it.

It cannot go anywhere else: cutover recreates a container with the mounts it
had and cannot invent a new one. The official image's own default is
`--config /etc/stalwart/config.json`, which is a *different* volume, so a
container left to that default would come up on whatever the old version had
left there. If your container overrides its command, cutover refuses rather
than merging the two — both are the container's argv and there is no honest
way to guess.

## What it keeps

The old container is renamed rather than removed, the old image is never
pruned, and the container's `docker inspect` is preserved as an artifact before
anything is replaced. Together those are the manual restore path — see
[recovery.md](recovery.md), which applies here exactly as it does to a binary
install.

Measured on a full migration: the store converts in seconds, and the service
was down for **6 seconds** end to end. Plan the window around verification,
not data volume.
