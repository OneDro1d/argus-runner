// Package schemas embeds the repo's schema files so the binary carries them.
//
// ⛔ WHY A PACKAGE HERE RATHER THAN A COPY UNDER internal/scenario: `go:embed` cannot reach a
// parent directory, and the alternative — a second copy of the schema beside the validator — is
// exactly the two-sources-of-truth shape VR12-S1 exists to remove. One file, one package, embedded
// once, imported by whoever needs it.
//
// The runner ships as a container and `schemas/` is not guaranteed to be beside the binary, so
// reading it from disk at run time is not an option either.
package schemas

import _ "embed"

// Scenario is schemas/scenario.schema.yaml. Its `x-markdown` block is the single source of truth
// for the closed markdown lists (VR12-S1); internal/scenario loads it at init.
//
//go:embed scenario.schema.yaml
var Scenario []byte

// ArgusConfig is schemas/argus-config.schema.yaml, embedded for the same reason.
//
//go:embed argus-config.schema.yaml
var ArgusConfig []byte
