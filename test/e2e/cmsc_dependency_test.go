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

	. "github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

var _ = Describe("CMSC dependency failure conditions", Label("cmsc", "dependency"), func() {
	BeforeEach(func() {
		ensureCMSCNotPaused()
	})

	// OP-E2E-007 — external DB probe failure (BYOI validation path).
	// Works on bundled lab clusters by temporarily setting deploy=false and an
	// unreachable host. Scaling bundled PostgreSQL to 0 does not flip the
	// condition: the operator treats 0 replicas as "ready" and only probes
	// external hosts when deploy=false.
	It("OP-E2E-007: database failure sets DatabaseReady=False", func() {
		restore := injectExternalDatabaseUnreachable()
		defer func() {
			restore()
			waitCMSCCondition(
				costv1alpha1.ConditionDatabaseReady, string(metav1.ConditionTrue), cmscMigrationWait,
				"DatabaseAvailable", "DatabaseReachable", "ExternalDatabase",
			)
			waitCMSCHealthyAfterDependencyRestore(cmscMigrationWait)
		}()

		By("pointing CMSC at unreachable external database (validation TCP probe)")
		waitCMSCCondition(
			costv1alpha1.ConditionDatabaseReady, string(metav1.ConditionFalse), cmscDependencyWait,
			"DatabaseUnreachable",
		)
		waitCMSCBlockingDependencyFailure(cmscDependencyWait)
	})

	// OP-E2E-008 — external cache probe failure.
	It("OP-E2E-008: cache failure sets CacheReady=False", func() {
		restore := injectExternalCacheUnreachable()
		defer func() {
			restore()
			waitCMSCCondition(
				costv1alpha1.ConditionCacheReady, string(metav1.ConditionTrue), cmscMigrationWait,
				"CacheAvailable", "CacheReachable", "ExternalCache",
			)
			waitCMSCHealthyAfterDependencyRestore(cmscMigrationWait)
		}()

		By("pointing CMSC at unreachable external cache (validation TCP probe)")
		waitCMSCCondition(
			costv1alpha1.ConditionCacheReady, string(metav1.ConditionFalse), cmscDependencyWait,
			"CacheUnreachable",
		)
		waitCMSCBlockingDependencyFailure(cmscDependencyWait)
	})

	// Optional: bundled infrastructure readiness (pod churn), not BYOI validation.
	// Set E2E_BUNDLED_INFRA_PROBE=1 to run after the primary external-probe tests.
	It("OP-E2E-007b: bundled database pod loss sets WaitingForDatabase", func() {
		if os.Getenv("E2E_BUNDLED_INFRA_PROBE") != "1" {
			Skip("set E2E_BUNDLED_INFRA_PROBE=1 to exercise bundled StatefulSet readiness")
		}
		cfg := getCMSC()
		if !databaseBundledDeployed(cfg) {
			Skip("database.deploy=false — use OP-E2E-007 external probe test")
		}
		stsName := databaseStatefulSetName()
		if !statefulSetExists(stsName) {
			Skip("bundled database StatefulSet not found")
		}

		By("deleting PostgreSQL pod to simulate brief bundled outage")
		deleteWorkloadPods("statefulset", stsName)

		waitCMSCCondition(
			costv1alpha1.ConditionDatabaseReady, string(metav1.ConditionFalse), cmscDependencyWait,
			"WaitingForDatabase",
		)
		waitCMSCCondition(
			costv1alpha1.ConditionDatabaseReady, string(metav1.ConditionTrue), cmscMigrationWait,
			"DatabaseAvailable",
		)
	})

	It("OP-E2E-008b: bundled cache pod loss sets WaitingForCache", func() {
		if os.Getenv("E2E_BUNDLED_INFRA_PROBE") != "1" {
			Skip("set E2E_BUNDLED_INFRA_PROBE=1 to exercise bundled cache readiness")
		}
		cfg := getCMSC()
		if !cacheBundledDeployed(cfg) {
			Skip("cache.deploy=false — use OP-E2E-008 external probe test")
		}
		kind := bundledCacheWorkloadKind()
		name := valkeyWorkloadName()
		if !bundledWorkloadExists(kind, name) {
			Skip(fmt.Sprintf("bundled Valkey workload %s/%s not found", kind, name))
		}

		By("deleting Valkey pod to simulate brief bundled outage (" + kind + ")")
		deleteWorkloadPods(kind, name)

		waitCMSCCondition(
			costv1alpha1.ConditionCacheReady, string(metav1.ConditionFalse), cmscDependencyWait,
			"WaitingForCache",
		)
		waitCMSCCondition(
			costv1alpha1.ConditionCacheReady, string(metav1.ConditionTrue), cmscMigrationWait,
			"CacheAvailable",
		)
	})
})
