package shim

import (
	"os"
	"testing"
)

// TestParseTargetMapDispatch pins that a plain single-target string
// (dynDrvStdenv's "/nonexistent/..." sentinel, or a bare target name)
// is never mistaken for the JSON-map format, and vice versa — this
// is the whole dispatch mechanism maybeSubmit/targetOutputKey rest
// on, with no separate env var to say which shape is in play.
func TestParseTargetMapDispatch(t *testing.T) {
	if got := parseTargetMap("/nonexistent/nixgg-phase1-no-per-artifact-submit"); got != nil {
		t.Errorf("plain sentinel string parsed as a JSON map: %v", got)
	}
	if got := parseTargetMap("mosh-server"); got != nil {
		t.Errorf("plain target name parsed as a JSON map: %v", got)
	}
	if got := parseTargetMap(""); got != nil {
		t.Errorf("empty string parsed as a JSON map: %v", got)
	}

	got := parseTargetMap(`{"mosh-server":"mosh-server.drv","mosh-client":"mosh-client.drv"}`)
	if got == nil {
		t.Fatal("valid JSON map failed to parse")
	}
	if got["mosh-server"] != "mosh-server.drv" || got["mosh-client"] != "mosh-client.drv" {
		t.Errorf("parseTargetMap = %v, want the two mosh entries", got)
	}
}

// TestTargetOutputKeySingleTarget pins that today's single-target
// shape (plain string, matched via matchesTarget) always resolves to
// "out" on a match — mkNixggBuild's existing single-target builds,
// and any caller of the old convention, must keep working unchanged.
func TestTargetOutputKeySingleTarget(t *testing.T) {
	t.Setenv("NIXGG_SANDBOX_TARGET", "mosh-server")
	if got := targetOutputKey("mosh-server"); got != "out" {
		t.Errorf("targetOutputKey(match) = %q, want \"out\"", got)
	}
	if got := targetOutputKey("mosh-client"); got != "" {
		t.Errorf("targetOutputKey(no match) = %q, want \"\"", got)
	}
}

// TestTargetOutputKeyMultiTarget pins the JSON-map format's own
// lookup: each pattern is matched the same way a plain string would
// be (matchesTarget's basename/relative/absolute rules), and the
// FIRST match's value is the real output key to report — "out" is
// never implied here, only whatever the map says.
func TestTargetOutputKeyMultiTarget(t *testing.T) {
	t.Setenv("NIXGG_SANDBOX_TARGET", `{"mosh-server":"mosh-server.drv","mosh-client":"mosh-client.drv"}`)
	if got := targetOutputKey("mosh-server"); got != "mosh-server.drv" {
		t.Errorf("targetOutputKey(mosh-server) = %q, want \"mosh-server.drv\"", got)
	}
	if got := targetOutputKey("mosh-client"); got != "mosh-client.drv" {
		t.Errorf("targetOutputKey(mosh-client) = %q, want \"mosh-client.drv\"", got)
	}
	if got := targetOutputKey("libmoshcrypto.a"); got != "" {
		t.Errorf("targetOutputKey(non-target archive) = %q, want \"\" — must not accidentally match", got)
	}
}

// TestTargetOutputKeyUnset pins that no NIXGG_SANDBOX_TARGET at all
// (native mode outside a shell replay, or any caller that never set
// it) never matches anything — maybeSubmit's own defaultSubmit param
// is what decides submission in that case, not this function.
func TestTargetOutputKeyUnset(t *testing.T) {
	os.Unsetenv("NIXGG_SANDBOX_TARGET")
	if got := targetOutputKey("anything"); got != "" {
		t.Errorf("targetOutputKey with no env var = %q, want \"\"", got)
	}
}

// TestMultiTargetNameFormula pins the actual naming contract
// submit-output enforces: outputPathName($name, key) == the
// submitted drv's own real name (drvName + "-" + key stripped of its
// own ".drv" suffix, since `nix derivation add` appends its OWN
// separate ".drv"). Confirmed directly against a real builder-rpc-v0
// sandbox before writing this — see multiTargetName's own docstring.
func TestMultiTargetNameFormula(t *testing.T) {
	t.Setenv("name", "nixgg-mosh")
	t.Setenv("NIXGG_SANDBOX_TARGET", `{"mosh-server":"mosh-server.drv","mosh-client":"mosh-client.drv"}`)

	if got := multiTargetName("mosh-server"); got != "nixgg-mosh-mosh-server" {
		t.Errorf("multiTargetName(mosh-server) = %q, want \"nixgg-mosh-mosh-server\"", got)
	}
	if got := multiTargetName("mosh-client"); got != "nixgg-mosh-mosh-client" {
		t.Errorf("multiTargetName(mosh-client) = %q, want \"nixgg-mosh-mosh-client\"", got)
	}
}

// TestMultiTargetNameNoOverride pins that multiTargetName returns ""
// — meaning "use the caller's own default bin-/ar- naming, no
// override" — for every case that ISN'T a matched non-"out" multi-
// target key: no env var set, a single-target "out" match, and a
// path that doesn't match any declared target at all (e.g. one of a
// multi-target build's own shared intermediate archives).
func TestMultiTargetNameNoOverride(t *testing.T) {
	t.Setenv("name", "nixgg-mosh")

	os.Unsetenv("NIXGG_SANDBOX_TARGET")
	if got := multiTargetName("mosh-server"); got != "" {
		t.Errorf("no env var: multiTargetName = %q, want \"\"", got)
	}

	t.Setenv("NIXGG_SANDBOX_TARGET", "mosh-server")
	if got := multiTargetName("mosh-server"); got != "" {
		t.Errorf("single-target \"out\" match: multiTargetName = %q, want \"\"", got)
	}

	t.Setenv("NIXGG_SANDBOX_TARGET", `{"mosh-server":"mosh-server.drv","mosh-client":"mosh-client.drv"}`)
	if got := multiTargetName("libmoshcrypto.a"); got != "" {
		t.Errorf("non-target shared archive: multiTargetName = %q, want \"\"", got)
	}
}
