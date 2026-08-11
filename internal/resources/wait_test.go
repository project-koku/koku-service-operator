package resources

import (
	"strings"
	"testing"
)

func TestIsValidHost(t *testing.T) {
	valid := []string{
		"postgres.databases.svc.cluster.local",
		"cost-management-database",
		"localhost",
		"10.0.0.1",
		"[::1]",
		"kafka-kafka-bootstrap.kafka.svc",
	}
	for _, h := range valid {
		if !isValidHost(h) {
			t.Errorf("isValidHost(%q) = false, want true", h)
		}
	}

	invalid := []string{
		"",
		`host"; rm -rf /`,
		"host$(evil)",
		"host`evil`",
		"host|evil",
		"host;evil",
		"host evil",
		"host\nevil",
	}
	for _, h := range invalid {
		if isValidHost(h) {
			t.Errorf("isValidHost(%q) = true, want false (injection risk)", h)
		}
	}
}

func TestIsValidPort(t *testing.T) {
	valid := []string{"1", "80", "443", "5432", "6379", "9092", "65535"}
	for _, p := range valid {
		if !isValidPort(p) {
			t.Errorf("isValidPort(%q) = false, want true", p)
		}
	}

	invalid := []string{"", "0", "65536", "-1", "abc", "80abc", "80;rm -rf /"}
	for _, p := range invalid {
		if isValidPort(p) {
			t.Errorf("isValidPort(%q) = true, want false", p)
		}
	}
}

func TestWaitForTCP_UsesWaitForBinary(t *testing.T) {
	// Set a known operator image so the test doesn't depend on env.
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	c := waitForTCP("wait-for-db", "db.svc", "5432")

	if c.Name != "wait-for-db" {
		t.Errorf("Name = %q", c.Name)
	}
	if c.Image != OperatorImage {
		t.Errorf("Image = %q, want operator image", c.Image)
	}
	if len(c.Command) == 0 || c.Command[0] != "/wait-for" {
		t.Errorf("Command[0] = %q, want /wait-for", c.Command[0])
	}
	// host and port must appear as separate argv elements after "--".
	args := c.Command
	sepIdx := -1
	for i, a := range args {
		if a == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("no -- separator in Command: %v", args)
	}
	if len(args) < sepIdx+3 {
		t.Fatalf("missing host/port after --: %v", args)
	}
	if args[sepIdx+1] != "db.svc" {
		t.Errorf("host arg = %q, want db.svc", args[sepIdx+1])
	}
	if args[sepIdx+2] != "5432" {
		t.Errorf("port arg = %q, want 5432", args[sepIdx+2])
	}
	// Confirm the full command has no shell script text at all.
	for _, a := range args {
		if strings.Contains(a, "/dev/tcp") || strings.Contains(a, "bash -c") {
			t.Errorf("shell pattern found in arg %q", a)
		}
	}
}

func TestWaitForTCP_InvalidHostFastFail(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	c := waitForTCP("test", `host";evil`, "5432")
	// Should use a short timeout so the pod fails fast on bad config.
	args := strings.Join(c.Command, " ")
	if !strings.Contains(args, "--timeout=1s") {
		t.Error("invalid host should produce --timeout=1s for fast failure")
	}
}

func TestWaitForHTTP_UsesWaitForBinary(t *testing.T) {
	old := OperatorImage
	OperatorImage = "quay.io/project-koku/koku-service-operator:test"
	defer func() { OperatorImage = old }()

	url := "http://cost-management-kruize:8080/listPerformanceProfiles"
	c := waitForHTTP("wait-for-kruize", url)

	if c.Name != "wait-for-kruize" {
		t.Errorf("Name = %q", c.Name)
	}
	if c.Image != OperatorImage {
		t.Errorf("Image = %q, want operator image", c.Image)
	}
	if len(c.Command) == 0 || c.Command[0] != "/wait-for" {
		t.Errorf("Command[0] = %q, want /wait-for", c.Command[0])
	}
	// URL must appear as a CLI arg, not be embedded in shell script text.
	lastArg := c.Command[len(c.Command)-1]
	if lastArg != url {
		t.Errorf("last arg = %q, want %q", lastArg, url)
	}
}
