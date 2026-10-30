package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWatchNamespace_NoSAFileNoEnv_ReturnsEmpty pins the *function* contract:
// watchNamespace() may return "" here. That empty is not itself a decision to
// start cluster-wide — the process-start guard lives in resolveWatchNamespace
// (see TestResolveWatchNamespace_OutOfClusterNoEnv_FailsClosed), which rejects
// this exact out-of-cluster case.
func TestWatchNamespace_NoSAFileNoEnv_ReturnsEmpty(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "")
	if got := watchNamespace(); got != "" {
		t.Errorf("expected empty (AllNamespaces) when no SA file and no env, got %q", got)
	}
}

// TestResolveWatchNamespace_OutOfClusterNoEnv_FailsClosed is the other half of
// the split: out-of-cluster with no pin must not start the process. An empty
// watch namespace there would list/watch every namespace via the laptop
// kubeconfig (typically cluster-admin).
func TestResolveWatchNamespace_OutOfClusterNoEnv_FailsClosed(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "")
	if ns, err := resolveWatchNamespace(); err == nil {
		t.Errorf("expected fail-closed error out-of-cluster with no pin, got ns=%q nil err", ns)
	}
}

// TestResolveWatchNamespace_InClusterEmpty_AllNamespaces confirms empty is still
// the legitimate AllNamespaces mode when in-cluster — the guard must not break it.
func TestResolveWatchNamespace_InClusterEmpty_AllNamespaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(path, []byte("cost-onprem\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceAccountNamespacePath = path
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "")
	ns, err := resolveWatchNamespace()
	if err != nil {
		t.Fatalf("in-cluster empty must be allowed (AllNamespaces), got error: %v", err)
	}
	if ns != "" {
		t.Errorf("in-cluster AllNamespaces must resolve empty, got %q", ns)
	}
}

// TestResolveWatchNamespace_OutOfClusterNAMESPACE_OK confirms a pinned laptop run
// is accepted (this is what `NAMESPACE=… make run` produces).
func TestResolveWatchNamespace_OutOfClusterNAMESPACE_OK(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "")
	t.Setenv("NAMESPACE", "cost-onprem")
	ns, err := resolveWatchNamespace()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns != "cost-onprem" {
		t.Errorf("expected cost-onprem, got %q", ns)
	}
}

// TestResolveWatchNamespace_OutOfClusterWatchNamespace_OK confirms WATCH_NAMESPACE
// also satisfies the pin requirement out-of-cluster.
func TestResolveWatchNamespace_OutOfClusterWatchNamespace_OK(t *testing.T) {
	serviceAccountNamespacePath = filepath.Join(t.TempDir(), "missing-namespace")
	t.Setenv("WATCH_NAMESPACE", "watch-me")
	t.Setenv("NAMESPACE", "")
	ns, err := resolveWatchNamespace()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns != "watch-me" {
		t.Errorf("expected watch-me, got %q", ns)
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
