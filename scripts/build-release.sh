#!/bin/bash
# SPDX-FileCopyrightText: 2026 Coffey Labs
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Build the release archives: one per architecture, plus SHA256SUMS.
#
# Usage: scripts/build-release.sh VERSION [OUTDIR]
#        scripts/build-release.sh v2026.9.15 dist
#
# The release workflow runs exactly this, so a release can be reproduced -- or
# checked before tagging -- on any machine with Go. Archive names carry no
# version, so .../releases/latest/download/<name> always means the newest.
set -euo pipefail

VERSION="${1:?usage: $0 VERSION [OUTDIR]}"
OUT="${2:-dist}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

rm -rf "$OUT" && mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# Linux only: the tool runs as root on the mail server it upgrades, which is a
# systemd service or a Docker container there.
for arch in amd64 arm64; do
  name="stalwart-migrate-linux-$arch"
  mkdir -p "$STAGE/$name"
  echo "==> building $name ($VERSION)"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$STAGE/$name/stalwart-migrate" ./cmd/stalwart-migrate)
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$STAGE/$name/"
  # Fixed owner and time, so the same commit gives the same archive.
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@${SOURCE_DATE_EPOCH:-0}" \
    -C "$STAGE/$name" -czf "$OUT/$name.tar.gz" stalwart-migrate LICENSE README.md
done

(cd "$OUT" && sha256sum ./*.tar.gz | sed 's| \./| |' > SHA256SUMS)
echo "==> $OUT:"
(cd "$OUT" && cat SHA256SUMS)
