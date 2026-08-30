// SPDX-FileCopyrightText: 2026 Coffey Labs
// SPDX-License-Identifier: GPL-3.0-or-later

package checkpoint

import "time"

// Phase identifies one of the top-level migration phases from
// ARCHITECTURE.md §4. Step records are scoped to a phase so the same step
// name can be reused across phases without colliding.
type Phase string

const (
	PhasePreflight Phase = "preflight"
	PhaseBackup    Phase = "backup"
	PhaseStage     Phase = "stage"
	PhaseRecovery  Phase = "recovery"
	PhaseCutover   Phase = "cutover"
	PhaseValidate  Phase = "validate"
)

// StepStatus is the lifecycle state of one checkpointed step.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
)

// StepOutcome is what a step reports back on success: a verdict
// classification the calling phase defines the meaning of (e.g. preflight's
// "ok"/"warn"/"fail"), a human-readable summary, and an optional
// machine-readable value later steps - or a resumed run reconstructing this
// step's result without re-executing it - need. Keeping these three
// separate (rather than one free-text field) is what lets `stalwart-migrate
// status` print a clean human summary while still round-tripping the data a
// resumed run depends on.
type StepOutcome struct {
	Verdict string `json:"verdict,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Extra   string `json:"extra,omitempty"`
}

// StepRecord captures one step's lifecycle status plus its StepOutcome.
type StepRecord struct {
	Phase       Phase      `json:"phase"`
	Name        string     `json:"name"`
	Status      StepStatus `json:"status"`
	StepOutcome            // embedded (untagged) so its fields flatten into this JSON object
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// Artifact is a content-addressed record of a file a run produced (a
// backup, a settings dump, a downloaded release binary) so later phases and
// a human operator can confirm it hasn't changed underfoot.
type Artifact struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// MailboxCount is one mailbox's message count as observed at preflight
// time, for later comparison against the post-migration count.
type MailboxCount struct {
	Mailbox  string `json:"mailbox"`
	Messages int    `json:"messages"`
}

// PreflightSnapshot holds the facts captured before anything is touched,
// which the validate phase later compares against the migrated instance.
// See ARCHITECTURE.md §4.1 and §4.7.
type PreflightSnapshot struct {
	TakenAt       time.Time                 `json:"taken_at"`
	AccountCount  int                       `json:"account_count"`
	Domains       []string                  `json:"domains,omitempty"`
	MailboxCounts map[string][]MailboxCount `json:"mailbox_counts,omitempty"` // account -> mailboxes
	// UsedQuota is each account's used storage in bytes. It is the only
	// per-account content measure available on both sides of the 0.15/0.16
	// boundary - 0.15.x exposes no per-mailbox message counts at all - so
	// it is what the post-migration comparison can actually assert on a
	// boundary migration. See stalwartapi/principal.go.
	UsedQuota        map[string]int64  `json:"used_quota,omitempty"`
	DKIMFingerprints map[string]string `json:"dkim_fingerprints,omitempty"`
	TLSFingerprints  []string          `json:"tls_fingerprints,omitempty"`
	ListenerPorts    []int             `json:"listener_ports,omitempty"`
}

// Topology records how this Stalwart instance is deployed, as detected
// during preflight, so the cutover phase knows whether it's managing a
// systemd unit or a container and what backend it's dealing with.
type Topology struct {
	DeploymentKind string   `json:"deployment_kind,omitempty"` // "systemd", "docker", "unknown"
	ClusterNodes   []string `json:"cluster_nodes,omitempty"`
	StoreBackend   string   `json:"store_backend,omitempty"`
	BlobStore      string   `json:"blob_store,omitempty"`
	FTSBackend     string   `json:"fts_backend,omitempty"`
}

// RunState is the full persisted state of one migration run: everything
// needed to resume it after a crash, or report on it later. See
// ARCHITECTURE.md §5.
type RunState struct {
	RunID             string              `json:"run_id"`
	SourceVersion     string              `json:"source_version"`
	TargetVersion     string              `json:"target_version"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
	Topology          Topology            `json:"topology,omitempty"`
	Steps             []StepRecord        `json:"steps"`
	Artifacts         map[string]Artifact `json:"artifacts,omitempty"`
	PreflightSnapshot *PreflightSnapshot  `json:"preflight_snapshot,omitempty"`
}

// RecordArtifact stores a content-addressed record of a file this run
// produced, keyed by a short logical name (e.g. "fs-backup", "settings-dump",
// "target-binary") rather than its path, since the path alone doesn't prove
// the content is what this run actually wrote.
func (rs *RunState) RecordArtifact(name string, a Artifact) {
	if rs.Artifacts == nil {
		rs.Artifacts = map[string]Artifact{}
	}
	rs.Artifacts[name] = a
}
