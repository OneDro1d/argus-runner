package packagecheck

import (
	"encoding/json"
	"fmt"
	"os"
)

// SchemaEntry is one declared variable: {name, required, secret, description}.
type SchemaEntry struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
	Description string `json:"description"`
}

// loadEnvSchema reads the env schema at root/rel (the conventional deploy/env.schema.json, or the
// one the deployment declared). A missing file is reported as a nil map and a nil error — the
// caller (envSchemaClause) turns that into a single, honest finding rather than an I/O error, since
// "the schema does not exist yet" is itself the completeness gap AC-9 checks for.
func loadEnvSchema(root, rel string) (map[string]SchemaEntry, error) {
	raw, err := os.ReadFile(under(root, rel))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []SchemaEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	out := make(map[string]SchemaEntry, len(entries))
	for _, e := range entries {
		out[e.Name] = e
	}
	return out, nil
}
