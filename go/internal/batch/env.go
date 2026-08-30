package batch

import "encoding/json"

// jsonGroup is the wire shape for one Group in $NIXGG_BATCH_GROUPS —
// a JSON array of {"name": ..., "patterns": [...]}, computed once at
// eval time by the Nix side (mirroring $NIXGG_KNOWN_STORE_PATHS's own
// convention — see toolchain.knownStorePathsFromEnv) so every shim
// invocation across the build sees byte-identical group definitions.
type jsonGroup struct {
	Name     string   `json:"name"`
	Patterns []string `json:"patterns"`
}

// FromJSON parses $NIXGG_BATCH_GROUPS's value. Returns a zero Config
// (no groups, Classify always returns ok=false) if s is empty or
// unparseable — same "absence is not an error" convention
// knownStorePathsFromEnv uses, since an empty manifest just means
// nothing is batched.
func FromJSON(s string) Config {
	if s == "" {
		return Config{}
	}
	var groups []jsonGroup
	if err := json.Unmarshal([]byte(s), &groups); err != nil {
		return Config{}
	}
	cfg := Config{Groups: make([]Group, len(groups))}
	for i, g := range groups {
		cfg.Groups[i] = Group{Name: g.Name, Patterns: g.Patterns}
	}
	return cfg
}
