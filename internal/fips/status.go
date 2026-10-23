// Package fips exposes the Go cryptographic module's runtime status.
package fips

import (
	"crypto/fips140"
	"encoding/json"
	"io"
)

// WriteStatus writes the running process's FIPS mode and module version as JSON.
func WriteStatus(w io.Writer) error {
	return json.NewEncoder(w).Encode(struct {
		Enabled bool   `json:"enabled"`
		Version string `json:"version"`
	}{
		Enabled: fips140.Enabled(),
		Version: fips140.Version(),
	})
}
