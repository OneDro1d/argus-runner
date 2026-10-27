package config

import (
	"fmt"
	"os"
)

// ExpandEnvRefs resolves the ${NAME} references in ONE string from the process environment, exactly as
// Load's expandEnv resolves a credential field: braces are required, ${cid}/${correlation_id} stay
// literal, and a name that is unset or empty is returned in missing (as "NAME") instead of being
// replaced by an empty string. It never returns a value in an error text.
//
// It exists for the one reader that must take a SINGLE field out of a config it parsed with
// ParseUnresolved: `argus collect-logs` needs observability.loki.credential and nothing else, and Load
// would refuse the whole file over the SUT's own ${VAR}s (bearer, database password) that this process
// is deliberately never given.
func ExpandEnvRefs(s string) (string, []string) {
	var missing []string
	out := envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := envVarRe.FindStringSubmatch(m)[1]
		if runtimeVars[name] {
			return m
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		missing = append(missing, name)
		return m
	})
	return out, missing
}

// ResolvedLokiCredential is LokiCredential for a config parsed with ParseUnresolved: the declared
// observability.loki.credential with its ${VAR}s resolved from the environment. An unresolvable
// reference is an error that names the variable, never the value.
func (c *Config) ResolvedLokiCredential() (string, error) {
	cred := c.Observability.Loki.Credential
	if cred == "" {
		return "", nil
	}
	out, missing := ExpandEnvRefs(cred)
	if len(missing) > 0 {
		return "", fmt.Errorf("observability.loki.credential references %v, which is not set in this process's environment", missing)
	}
	return out, nil
}
