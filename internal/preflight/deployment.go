package preflight

import (
	"context"
	"os"
	"os/exec"
)

// DeploymentKind is how a Stalwart instance appears to be run, which
// determines how cutover and rollback restart it.
type DeploymentKind string

const (
	DeploymentSystemd DeploymentKind = "systemd"
	DeploymentDocker  DeploymentKind = "docker"
	DeploymentUnknown DeploymentKind = "unknown"
)

var systemdUnitPaths = []string{
	"/etc/systemd/system/stalwart.service",
	"/lib/systemd/system/stalwart.service",
	"/usr/lib/systemd/system/stalwart.service",
}

// DetectDeploymentKind makes a best-effort guess at how Stalwart is run
// here. It's deliberately conservative and cheap (file stats, one docker
// inspect) rather than exhaustive - an operator-supplied override should
// always be able to win over this, since the cost of guessing wrong here is
// cutover targeting the wrong thing.
func DetectDeploymentKind(ctx context.Context, containerName string) DeploymentKind {
	for _, p := range systemdUnitPaths {
		if _, err := os.Stat(p); err == nil {
			return DeploymentSystemd
		}
	}
	if containerName == "" {
		containerName = "stalwart"
	}
	if _, err := exec.LookPath("docker"); err == nil {
		if err := exec.CommandContext(ctx, "docker", "inspect", containerName).Run(); err == nil {
			return DeploymentDocker
		}
	}
	return DeploymentUnknown
}
