//go:build nativefips

// The Red Hat toolchain supports automatic activation from the host FIPS state.
// Keep this default out of ordinary builds with the upstream Go toolchain.
//go:debug fips140=auto

package main
