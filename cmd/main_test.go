package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWatchNamespace_NoSAFileNoEnv_ReturnsEmpty(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "")
	if got := watchNamespace(); got != "" {
		t.Errorf("expected empty (AllNamespaces) when no SA file and no env, got %q", got)
	}
}

func TestWatchNamespace_FromNAMESPACEOutOfCluster(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "cost-onprem")
	if got := watchNamespace(); got != "cost-onprem" {
		t.Errorf("expected cost-onprem, got %q", got)
	}
}

func TestWatchNamespace_WATCH_NAMESPACEWins(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "watch-me")
	t.Setenv("NAMESPACE", "cost-onprem")
	if got := watchNamespace(); got != "watch-me" {
		t.Errorf("expected WATCH_NAMESPACE to win, got %q", got)
	}
}

func TestWatchNamespace_InClusterIgnoresNAMESPACEAndSAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("  koku-service-operator-system\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceAccountNamespacePath = path
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "should-not-pin")
	if got := watchNamespace(); got != "" {
		t.Errorf("in-cluster AllNamespaces must not pin to SA file or NAMESPACE, got %q", got)
	}
}

func TestWatchNamespace_InClusterWATCH_NAMESPACEPins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namespace")
	if err := os.WriteFile(path, []byte("cost-onprem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceAccountNamespacePath = path
	t.Setenv("WATCH_NAMESPACE", "only-this")
	t.Setenv("NAMESPACE", "cost-onprem")
	if got := watchNamespace(); got != "only-this" {
		t.Errorf("expected WATCH_NAMESPACE pin in-cluster, got %q", got)
	}
}

func TestCacheOptionsForNamespace_EmptyIsClusterWide(t *testing.T) {
	opts := cacheOptionsForNamespace("")
	if opts.DefaultNamespaces != nil {
		t.Errorf("empty ns must leave DefaultNamespaces unset, got %+v", opts.DefaultNamespaces)
	}
}

func TestCacheOptionsForNamespace_Pins(t *testing.T) {
	opts := cacheOptionsForNamespace("cost-onprem")
	if _, ok := opts.DefaultNamespaces["cost-onprem"]; !ok {
		t.Errorf("expected DefaultNamespaces[cost-onprem], got %+v", opts.DefaultNamespaces)
	}
}
