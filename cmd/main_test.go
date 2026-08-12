package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWatchNamespace_NoSAFileNoEnv_ReturnsEmpty(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("NAMESPACE", "")
	if got := watchNamespace(); got != "" {
		t.Errorf("expected empty when no SA file and no NAMESPACE, got %q", got)
	}
}

func TestWatchNamespace_FromEnv(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("NAMESPACE", "cost-onprem")
	if got := watchNamespace(); got != "cost-onprem" {
		t.Errorf("expected cost-onprem, got %q", got)
	}
}

func TestWatchNamespace_EmptyEnv(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("NAMESPACE", "")
	if got := watchNamespace(); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestWatchNamespace_FromSAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("  cost-onprem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceAccountNamespacePath = path
	t.Setenv("NAMESPACE", "should-not-win")
	if got := watchNamespace(); got != "cost-onprem" {
		t.Errorf("expected SA file to win over NAMESPACE, got %q", got)
	}
}

func TestWatchNamespace_BlankSAFileFallsBackToEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceAccountNamespacePath = path
	t.Setenv("NAMESPACE", "cost-byoi")
	if got := watchNamespace(); got != "cost-byoi" {
		t.Errorf("expected NAMESPACE fallback, got %q", got)
	}
}
