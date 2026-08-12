package main

import (
	"testing"
)

func TestParseTarget_NoArgs(t *testing.T) {
	_, err := parseTarget(nil)
	if err == nil {
		t.Fatal("expected error with no arguments")
	}
}

func TestParseTarget_TCP(t *testing.T) {
	tgt, err := parseTarget([]string{"db.svc", "5432"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.mode != modeTCP {
		t.Errorf("mode = %v, want modeTCP", tgt.mode)
	}
	if tgt.addr != "db.svc:5432" {
		t.Errorf("addr = %q, want db.svc:5432", tgt.addr)
	}
}

func TestParseTarget_HTTP(t *testing.T) {
	url := "http://kruize:8080/health"
	tgt, err := parseTarget([]string{url})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.mode != modeHTTP {
		t.Errorf("mode = %v, want modeHTTP", tgt.mode)
	}
	if tgt.addr != url {
		t.Errorf("addr = %q, want %q", tgt.addr, url)
	}
}

func TestParseTarget_HTTPS(t *testing.T) {
	url := "https://secure.svc:8443/ready"
	tgt, err := parseTarget([]string{url})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tgt.mode != modeHTTP {
		t.Errorf("mode = %v, want modeHTTP", tgt.mode)
	}
	if tgt.addr != url {
		t.Errorf("addr = %q, want %q", tgt.addr, url)
	}
}

func TestParseTarget_TooManyArgs(t *testing.T) {
	_, err := parseTarget([]string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error with 3 non-URL arguments")
	}
}

func TestParseTarget_SingleNonURL(t *testing.T) {
	_, err := parseTarget([]string{"not-a-url"})
	if err == nil {
		t.Fatal("expected error with single non-URL argument")
	}
}
