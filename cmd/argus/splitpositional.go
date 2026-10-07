package main

import "strings"

// splitPositional separates args into (flag tokens, positional tokens), regardless of how they are
// interleaved. Every flag of the commands that use it is string-valued except --json, so
// "--name value" is a pair and "--json" stands alone. Go's flag.Parse stops at the FIRST non-flag token
// and treats everything after it as positional, which would silently strip every flag typed after
// `argus certificate verify <file>` (a positional-first usage shape); this makes the order the caller
// chose irrelevant.
func splitPositional(args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if strings.Contains(a, "=") || a == "--json" || a == "-json" {
				continue
			}
			if i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return flagArgs, positional
}
