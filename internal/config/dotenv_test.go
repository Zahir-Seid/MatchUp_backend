package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDotEnvSetsMissingAndKeepsExisting locks in the precedence that makes
// auto-loading safe: a key only the file has is filled in, while a key the
// environment already supplies is left alone.
func TestLoadDotEnvSetsMissingAndKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	const (
		fromFile = "MATCHUP_DOTENV_FROM_FILE"
		fromEnv  = "MATCHUP_DOTENV_FROM_ENV"
	)
	contents := fromFile + "=file-value\n" + fromEnv + "=file-value\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// The file-only key must start unset, and must not leak into other tests.
	old, had := os.LookupEnv(fromFile)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(fromFile, old)
			return
		}
		_ = os.Unsetenv(fromFile)
	})
	if err := os.Unsetenv(fromFile); err != nil {
		t.Fatalf("unset %s: %v", fromFile, err)
	}
	t.Setenv(fromEnv, "env-value")

	LoadDotEnv()

	if got := os.Getenv(fromFile); got != "file-value" {
		t.Errorf("%s = %q, want file-value", fromFile, got)
	}
	if got := os.Getenv(fromEnv); got != "env-value" {
		t.Errorf("%s = %q, want env-value (the environment must win)", fromEnv, got)
	}
}

// TestLoadDotEnvMissingFileIsNoOp requires a missing .env to be ordinary:
// production images do not carry one, and booting must not depend on it.
func TestLoadDotEnvMissingFileIsNoOp(t *testing.T) {
	t.Chdir(t.TempDir())
	LoadDotEnv()
}
