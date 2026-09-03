package resources

import (
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// OperatorImage is the image used for wait-for init containers.
// It is set at startup from the --operator-image CLI flag (see cmd/main.go).
// The flag is required; the operator exits at startup if it is empty.
// Set it to the fully-qualified image reference of the operator pod, e.g.:
//
//	--operator-image=quay.io/project-koku/koku-service-operator:v1.0.0
//
// In OLM deployments the CSV sets this via the Deployment args.
// For local development: make run (which passes --operator-image=$(IMG)).
//
// The empty-string default allows test code to set the variable directly
// without going through flag.Parse().
var OperatorImage = os.Getenv("OPERATOR_IMAGE") // overwritten by --operator-image flag

// waitForTCP returns an init container that blocks until host:port accepts
// TCP connections.
//
// Security model:
//   - host and port are passed as positional parameters ($1, $2) via the
//     execve() argument vector — never interpolated into the script text.
//   - Both are validated in Go before the container spec is built; an invalid
//     value produces an immediate, visible pod failure.
//
// TODO(COST-future): replace bash+/dev/tcp with a purpose-built Go binary —
// see docs/tasks.md "waitForTCP Go binary".
// waitForTCP returns an init container that blocks until host:port accepts TCP.
// Uses /wait-for from the operator image (set via OPERATOR_IMAGE Downward API).
// host and port are joined as "tcp://host:port" and passed as a single CLI arg
// to the Go binary — no shell, no injection surface.
// isValidHost/isValidPort are checked first so wait4x produces a readable error
// quickly rather than retrying for ten minutes on a garbage address.
func waitForTCP(containerName, host, port string) corev1.Container {
	timeout := "--timeout=10m"
	if !isValidHost(host) || !isValidPort(port) {
		timeout = "--timeout=1s" // fail fast on obviously bad config
	}
	return corev1.Container{
		Name:            containerName,
		Image:           OperatorImage,
		Command:         []string{"/wait-for", timeout, "--", host, port},
		SecurityContext: restrictedContainerSC(),
	}
}

// waitForHTTP returns an init container that blocks until a URL returns HTTP 200.
// Use this instead of waitForTCP when the service requires a full HTTP handshake
// before it is ready (e.g. Kruize, which needs the JVM fully initialised).
// Note: wait4x.dev/v3 WithExpectStatusCode(200) accepts only 200, not the full
// 2xx range. Use a URL that returns exactly 200 for readiness.
func waitForHTTP(containerName, url string) corev1.Container {
	return corev1.Container{
		Name:            containerName,
		Image:           OperatorImage,
		Command:         []string{"/wait-for", "--interval=5s", "--timeout=10m", url},
		SecurityContext: restrictedContainerSC(),
	}
}

// waitForPostgres returns an init container that blocks until the database
// is ready to accept connections.
//
// When the bundled Postgres is deployed, pg_isready from the database image is
// used — it validates the full application-level handshake (auth ready, not
// replaying WAL) rather than just a raw TCP accept. host and port are passed
// as positional argv — never interpolated into the shell script.
//
// For external databases we fall back to the generic TCP check since we do not
// control which image is available in that environment.
func waitForPostgres(cfg *costv1alpha1.CostManagementServiceConfig, dbHost, dbPort string) corev1.Container {
	if costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, false) {
		img, _ := ImageRef(cfg.Spec.Database.Image)
		return corev1.Container{
			Name:  "wait-for-postgres",
			Image: img,
			Command: []string{
				"/bin/sh", "-c",
				`until pg_isready -h "$1" -p "$2"
do printf 'waiting for postgres %s:%s\n' "$1" "$2"; sleep 2; done`,
				"--", dbHost, dbPort,
			},
			SecurityContext: restrictedContainerSC(),
		}
	}
	return waitForTCP("wait-for-db", dbHost, dbPort)
}

// isValidHost returns true when host contains only characters safe for
// /dev/tcp paths: RFC 1123 labels, dots, hyphens, and brackets for IPv6.
func isValidHost(host string) bool {
	if host == "" {
		return false
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '.', c == '[', c == ']', c == ':':
			// valid
		default:
			return false
		}
	}
	return true
}

// isValidPort returns true when port is a decimal integer in [1, 65535].
func isValidPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}
