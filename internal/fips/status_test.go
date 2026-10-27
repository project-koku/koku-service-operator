package fips

import (
	"bytes"
	"crypto/fips140"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

// The runtime fixes FIPS mode during initialization, so exercise each mode in a
// fresh process instead of changing GODEBUG inside the current test process.
func TestWriteStatusRuntimeModes(t *testing.T) {
	if os.Getenv("FIPS_STATUS_TEST_PROCESS") == "1" {
		if err := WriteStatus(os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	strictHost := strictFIPSHost(t)
	for _, mode := range []string{"on", "off"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWriteStatusRuntimeModes$")
			cmd.Env = append(os.Environ(), "FIPS_STATUS_TEST_PROCESS=1", "GODEBUG=fips140="+mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if mode == "off" && strictHost {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("expected strict FIPS startup rejection, got error %v, stdout %q, stderr %q", err, output, stderr.String())
				}
				const diagnostic = "Host FIPS mode is enabled, but the required GODEBUG=fips140 module is disabled"
				if len(output) != 0 || strings.TrimSpace(stderr.String()) != diagnostic {
					t.Fatalf("unexpected strict FIPS rejection: stdout %q, stderr %q", output, stderr.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("run status with fips140=%s: %v; stderr: %s", mode, err, stderr.String())
			}
			var status struct {
				Enabled bool   `json:"enabled"`
				Version string `json:"version"`
			}
			decoder := json.NewDecoder(bytes.NewReader(output))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&status); err != nil {
				t.Fatalf("decode status %q: %v", output, err)
			}
			if status.Enabled != (mode == "on") {
				t.Errorf("enabled = %v for fips140=%s", status.Enabled, mode)
			}
			if status.Version != fips140.Version() {
				t.Errorf("version = %q, want %q", status.Version, fips140.Version())
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				t.Errorf("unexpected output after status: %v", err)
			}
		})
	}
}

// Match the release toolchain's strict-startup conditions, using the executable's
// build settings rather than GOEXPERIMENT from its runtime environment.
func strictFIPSHost(t *testing.T) bool {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("cannot read test executable build settings")
	}
	strict := false
	for _, setting := range info.Settings {
		if setting.Key == "GOEXPERIMENT" {
			strict = slices.Contains(strings.Split(setting.Value, ","), "strictfipsruntime")
		}
	}
	if !strict {
		return false
	}
	if os.Getenv("GOLANG_NATIVE_HOSTFIPS_OVERRIDE") == "1" {
		return true
	}
	state, err := os.ReadFile("/proc/sys/crypto/fips_enabled")
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("read host FIPS state: %v", err)
	}
	return len(state) > 0 && state[0] == '1'
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteStatusWriterError(t *testing.T) {
	want := errors.New("output unavailable")
	if err := WriteStatus(failingWriter{err: want}); !errors.Is(err, want) {
		t.Errorf("WriteStatus error = %v, want %v", err, want)
	}
}
