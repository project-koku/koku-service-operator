//go:build cluster_e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestCMSCE2E runs operator lifecycle scenarios (COST-7698) against a pre-deployed
// reconciled stack. Skipped unless E2E_CLUSTER=1 (Kind manager smoke stays in TestE2E).
//
// Prerequisites and env vars: docs/development/cmsc-e2e.md
func TestCMSCE2E(t *testing.T) {
	if os.Getenv("E2E_CLUSTER") != "1" {
		t.Skip("skipping cluster CMSC e2e (set E2E_CLUSTER=1 against a reconciled cost-onprem stack)")
	}
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting CMSC operator lifecycle e2e (COST-7698)\n")
	RunSpecs(t, "CMSC operator lifecycle e2e")
}

var _ = BeforeSuite(func() {
	initCMSCTestEnv()
	waitCMSCHealthyGate()
})

var _ = AfterEach(func() {
	if CurrentSpecReport().Failed() {
		collectCMSCForensics()
	}
})
