package federation

// VersionReading (migration 077) is what an executor saw of ONE Kubernetes namespace at
// the start of a run: which version of the system under test the run was about to exercise there.
//
// ⛔ IT HOLDS NO POD NAME, NO IMAGE NAME AND NO ENVIRONMENT, and the type has no field for one: the key is a
// hash over the running image digests (artifactmeasure.VersionKey), and only the counts and a short reason
// travel beside it. The control plane decodes into this type and stores the RE-ENCODING, so a key a newer or
// hostile executor adds is cut on the way in.
type VersionReading struct {
	// Namespace is the Kubernetes namespace read; "" on the compose tier (one reading, no Kubernetes call).
	Namespace string `json:"namespace"`
	// Targets are the test_targets names whose scenarios resolved to this namespace in this run (none when
	// the instance declares no test_targets).
	Targets []string `json:"targets,omitempty"`
	// Key is "" when the namespace could not be read or no container reported a digest. It depends ONLY on the
	// set of digests: not on their order, duplicates, replica counts or resource limits.
	Key string `json:"key,omitempty"`
	// Digests is the number of distinct image digests the key covers; Unresolved the containers that
	// reported none (a tag-only or locally built image).
	Digests    int `json:"digests"`
	Unresolved int `json:"unresolved"`
	// Reason says why there is no Key. It names the namespace.
	Reason string `json:"reason,omitempty"`
}
