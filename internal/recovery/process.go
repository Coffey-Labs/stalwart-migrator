// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// maxCapturedOutput bounds what Process keeps from the child's stdout and
// stderr. Stalwart logs continuously once it is up, so this keeps the most
// recent output rather than the whole session - which is also what a
// failure needs, since the reason a server died is at the end of its log.
const maxCapturedOutput = 64 << 10

// outputBuffer collects the child process's combined output for use in
// error messages. exec.Cmd writes to it from its own goroutine while the
// supervising goroutine may read it, hence the mutex.
type outputBuffer struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > maxCapturedOutput {
		b.buf = b.buf[len(b.buf)-maxCapturedOutput:]
		b.truncated = true
	}
	return len(p), nil
}

func (b *outputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return "...(earlier output truncated)...\n" + string(b.buf)
	}
	return string(b.buf)
}

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
//
// It captures the child's output. That is not a nicety: a smoke test
// against a real 0.15.5 instance spent several rounds diagnosing a recovery
// boot that failed because port 8080 was already in use, and the tool
// reported only "connection refused" after a 60-second timeout - while
// Stalwart had printed "Failed to bind to [::]:8080: Address already in
// use" immediately, to a pipe nothing was reading. Anything that reports a
// supervised process failing must be able to say why.
type Process struct {
	cmd    *exec.Cmd
	output *outputBuffer
}

// Output returns what the child has written to stdout and stderr so far,
// most-recent-first-truncated if it exceeded maxCapturedOutput. Safe to
// call at any point, including before Start and after Stop.
func (p *Process) Output() string {
	if p.output == nil {
		return ""
	}
	return p.output.String()
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
	return p.start(cmd, o.BinaryPath)
}

// start attaches the output buffer and launches cmd. Split out of Start so
// a deployment that runs the target version some other way - a container,
// where the child is `docker run` rather than the server itself - gets the
// same supervision: the same captured output, and the same Stop.
//
// what names the thing being started, for the error if it will not.
func (p *Process) start(cmd *exec.Cmd, what string) error {
	p.output = &outputBuffer{}
	cmd.Stdout = p.output
	cmd.Stderr = p.output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("recovery: start %s: %w", what, err)
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
	// A process that already exited on its own still has to be reaped:
	// cmd.Wait is what collects its status and, crucially, waits for the
	// goroutines copying its output into our buffer. Returning early here
	// would discard that output in exactly the case where it matters most -
	// the server died by itself and its log says why.
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
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
