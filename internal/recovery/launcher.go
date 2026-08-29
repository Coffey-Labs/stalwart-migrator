// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"time"
)

// Supervised is one running instance of the target Stalwart version,
// however it was started. The two phases that bring the target version up
// against a not-yet-migrated store - the recovery cycle here, and
// validate.BootCheck's ordinary boot afterwards - need exactly three things
// from it: to stop it, to read what it printed, and (via WaitForHealthy,
// which only needs a URL) to know when it is ready.
//
// Output is not a nicety. A recovery boot that failed because port 8080 was
// already in use reported only "connection refused" after a 60-second
// timeout, while Stalwart had printed "Address already in use" immediately
// to a pipe nothing was reading. Anything reporting a supervised process
// failing has to be able to say why, whatever started it.
type Supervised interface {
	Stop(gracePeriod time.Duration) error
	Output() string
}

// LaunchOptions is everything about running the target version that does
// not depend on how it is packaged. What differs by packaging - a path on
// this host, or an image and the mounts to run it against - belongs to the
// Launcher, which was constructed knowing it.
type LaunchOptions struct {
	ConfigPath string

	// RecoveryMode sets STALWART_RECOVERY_MODE=1 and
	// STALWART_RECOVERY_ADMIN=<AdminUser>:<AdminPassword>, the environment
	// Stalwart's own upgrade guide uses to bring the new version up against
	// an unmigrated store. False means an ordinary boot.
	RecoveryMode  bool
	AdminUser     string
	AdminPassword string

	// ExtraEnv is appended after any recovery-mode variables, so a
	// rehearsal can point ports and paths at a sandbox without those two
	// becoming the only way to parameterize the launch.
	ExtraEnv []string
}

// Launcher starts the staged target version. It exists because that is the
// one thing in this phase that packaging changes: a systemd install runs a
// binary this tool downloaded, a container runs an image against the data
// volume, and everything either of them is started *for* - the recovery
// cycle, the settings apply, the boot check - is identical afterwards.
//
// See ARCHITECTURE.md §4.4. The container implementation is issue #3.
type Launcher interface {
	// Launch starts the instance and returns once the OS reports it
	// started. It does not wait for Stalwart to be ready: callers use
	// WaitForHealthy for that, because readiness is an HTTP question and
	// the same one either way.
	Launch(ctx context.Context, o LaunchOptions) (Supervised, error)
}

// BinaryLauncher runs the target version as a child process from a path on
// this host - the systemd deployment's answer, and the only one until
// container support lands.
type BinaryLauncher struct {
	BinaryPath string
}

func (b BinaryLauncher) Launch(ctx context.Context, o LaunchOptions) (Supervised, error) {
	p := &Process{}
	if err := p.Start(ctx, ProcessOptions{
		BinaryPath:    b.BinaryPath,
		ConfigPath:    o.ConfigPath,
		RecoveryMode:  o.RecoveryMode,
		AdminUser:     o.AdminUser,
		AdminPassword: o.AdminPassword,
		ExtraEnv:      o.ExtraEnv,
	}); err != nil {
		return nil, err
	}
	return p, nil
}

// Process satisfies Supervised. Asserted here rather than left to the call
// sites so the compiler catches it if either side drifts.
var _ Supervised = (*Process)(nil)
var _ Launcher = BinaryLauncher{}
