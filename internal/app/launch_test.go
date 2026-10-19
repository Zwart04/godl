package app

import (
	"strings"
	"testing"
	"time"

	"godl/internal/config"
)

// The launch path is what a browser hits when someone clicks a link before
// ever opening godl. Both halves of it matter: it must find the program it is
// meant to start, and it must refuse rather than guess when it cannot.
func TestEnsureRunningExplainsWhenGodlIsMissing(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	err := EnsureRunning(time.Second)
	if err == nil {
		t.Fatal("wanted an error when godl.exe is not next to the running program")
	}
	if !strings.Contains(err.Error(), "godl.exe") {
		t.Errorf("error = %q, want it to name the file it could not find", err.Error())
	}
}

func TestManagerExeIsNeverGuessed(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	exe, err := managerExe()
	if err == nil {
		t.Fatalf("managerExe() = %q, want a failure outside a real install", exe)
	}
}

func TestStartTimeoutHasAFloor(t *testing.T) {
	// A zero or negative wait must not turn into an immediate failure that
	// looks like "godl refuses to start".
	if StartTimeout < 10*time.Second {
		t.Errorf("StartTimeout = %v, want it long enough for a cold start", StartTimeout)
	}
}
