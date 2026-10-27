package report

// Fault is one fault a chain's `fault` step applied to the system under test, and how it
// was undone. A deployment is named by its `fault_allowed_targets` name and a link by its allowlist name:
// NO URL, namespace or Toxiproxy coordinate is ever recorded here.
type Fault struct {
	// Step is the chain step that applied it. Scenario-internal: it is NOT carried to the certificate.
	Step string `json:"step"`
	// Op is stop | scale | delay | drop | truncate. (start, wait_ready and restore apply no fault; a
	// `start` or `restore` step closes the entry an earlier step opened.)
	Op string `json:"op"`
	// Target is the allowlist name of the deployment or the link.
	Target string `json:"target"`
	// Params are the op's numbers: replicas, latency_ms, jitter_ms, bytes. Always integers.
	Params     map[string]int `json:"params,omitempty"`
	AppliedAt  string         `json:"applied_at"` // RFC3339 UTC
	RestoredAt string         `json:"restored_at,omitempty"`
	// RestoreOutcome is FaultRestored | FaultAlreadyRestored | FaultRestoreFailed. A fault still active
	// when the record is read has none; one is always set once a run has finished.
	RestoreOutcome string `json:"restore_outcome"`
	// Toxiproxy says a Toxiproxy was in the path of the connection while this fault ran (a link fault).
	Toxiproxy bool `json:"toxiproxy,omitempty"`
}

const (
	// FaultRestored - the undo ran and changed the system back.
	FaultRestored = "restored"
	// FaultAlreadyRestored - the undo found nothing to undo (the toxic was gone, the deployment already
	// had the recorded count). A success, for the reason cleanup rule 3 gives: a safety net that reds on
	// "already gone" gets switched off.
	FaultAlreadyRestored = "already-restored"
	// FaultRestoreFailed - the undo did NOT complete. LOUD: the system may still be broken.
	FaultRestoreFailed = "failed"
)
