// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ProcessOptions configures how the target binary is launched.
type ProcessOptions struct {
	BinaryPath string
	ConfigPath string

	// RecoveryMode, when true, sets STALWART_RECOVERY_MODE=1 and
	// STALWART_RECOVERY_ADMIN=<AdminUser>:<AdminPassword> - exactly the
	// environment variables Stalwart's own upgrade guide uses to bring the
	// new binary up in recovery mode against a not-yet-migrated store (see
	// UPGRADING/v0_16.md). When false, the binary is started as a normal
	// boot against ConfigPath - used after recovery mode to confirm the
	// migrated store comes up cleanly under an ordinary start, not just a
	// recovery one (see ARCHITECTURE.md's dry-run design).
	RecoveryMode  bool
	AdminUser     string
	AdminPassword string

	// ExtraEnv is appended after any recovery-mode env vars. This is what
	// lets a dry-run point the process at a sandbox without RecoveryMode's
	// two env vars becoming the only way to parameterize the child process.
	ExtraEnv []string
}

// Process supervises one run of the target Stalwart binary as a background
// child process, so a caller can start it, wait for it to become healthy,
// interact with it, and stop it again - without ever touching a real
// systemd unit or Docker container. See ARCHITECTURE.md §4.4.
type Process struct {
	cmd *exec.Cmd
}

// Start launches the binary. It returns as soon as the OS has started the
// process - it does not wait for Stalwart itself to become ready; use
// WaitForHealthy for that.
func (p *Process) Start(ctx context.Context, o ProcessOptions) error {
	var env []string
	if o.RecoveryMode {
		env = append(env,
			"STALWART_RECOVERY_MODE=1",
			fmt.Sprintf("STALWART_RECOVERY_ADMIN=%s:%s", o.AdminUser, o.AdminPassword),
		)
	}
	env = append(env, o.ExtraEnv...)

	cmd := exec.CommandContext(ctx, o.BinaryPath, "--config", o.ConfigPath)
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("recovery: start %s: %w", o.BinaryPath, err)
	}
	p.cmd = cmd
	return nil
}

// Stop sends SIGTERM and waits up to gracePeriod for the process to exit -
// mirroring the upgrade guide's own "Ctrl+C in the first terminal" step,
// just automated - escalating to SIGKILL if it doesn't exit in time so a
// stuck child process can never hang a migration run indefinitely. Safe to
// call on a Process that was never successfully started.
func (p *Process) Stop(gracePeriod time.Duration) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("recovery: signal process (pid %d): %w", p.cmd.Process.Pid, err)
	}

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()

	select {
	case err := <-done:
		// A non-zero exit from a SIGTERM-based shutdown is expected and not
		// itself a failure worth reporting - only an unexpected Wait error is.
		if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				return fmt.Errorf("recovery: wait for process (pid %d): %w", p.cmd.Process.Pid, err)
			}
		}
		return nil
	case <-time.After(gracePeriod):
		_ = p.cmd.Process.Kill()
		<-done
		return fmt.Errorf("recovery: process (pid %d) did not exit within %s of SIGTERM - sent SIGKILL", p.cmd.Process.Pid, gracePeriod)
	}
}
