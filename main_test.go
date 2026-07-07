package main

import (
	"testing"

	"houston/internal/module"
)

// The envelope's houston.version and HOUSTON_VERSION must always match what
// `houston version` prints — init() stamps the module package's copy, and
// this pins the wiring (a release build only differs by the ldflags value).
func TestModuleVersionStamped(t *testing.T) {
	if module.HoustonVersion != version {
		t.Fatalf("module.HoustonVersion = %q, want %q", module.HoustonVersion, version)
	}
}
