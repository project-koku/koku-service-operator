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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

var _ = Describe("CMSC upgrade sequencing", Label("cmsc", "upgrade"), func() {
	BeforeEach(func() {
		ensureCMSCNotPaused()
	})

	// OP-E2E-005 — upgrade: migrate Job completes, then Deployment rolls forward.
	It("OP-E2E-005: Koku upgrade migration before API rollout", func() {
		upgradeTag := os.Getenv("E2E_KOKU_UPGRADE_TAG")
		if upgradeTag == "" {
			Skip("set E2E_KOKU_UPGRADE_TAG to a newer lab image tag (must differ from current CMSC tag)")
		}

		cfg := getCMSC()
		originalTag := cfg.Spec.CostManagement.API.Image.Tag
		if originalTag == upgradeTag {
			Skip("E2E_KOKU_UPGRADE_TAG must differ from current CMSC tag")
		}
		repo := cfg.Spec.CostManagement.API.Image.Repository
		depName := kokuAPIDeploymentName()
		originalImage := deploymentContainerImage(depName, "koku-api")

		defer restoreKokuImageTag(repo, originalTag)

		By("patching CMSC Koku API image tag (upgrade)")
		patchCMSCImageKoku(repo, upgradeTag)

		waitMigrationJobStarted(kokuMigrationJobName(), upgradeTag)
		assertRolloutBlockedDuringMigration(kokuMigrationJobName(), upgradeTag, depName, "koku-api", originalImage)

		By("waiting for successful migration Job completion")
		Eventually(migrationJobCompleteForTag, cmscMigrationWait, 15*time.Second).
			WithArguments(kokuMigrationJobName(), upgradeTag).Should(BeTrue())
		waitCMSCCondition(costv1alpha1.ConditionSchemaUpToDate, string(metav1.ConditionTrue), cmscMigrationWait, "MigrationComplete")

		By("verifying Deployment rolled to upgraded image")
		Eventually(func(g Gomega) {
			g.Expect(deploymentContainerImage(depName, "koku-api")).To(ContainSubstring(upgradeTag))
		}, cmscMigrationWait, 15*time.Second).Should(Succeed())
	})

	// OP-E2E-005b — downgrade: migration must gate rollout even when schema migrate fails.
	// On lab cluster, 768be82 → 5432d06 hits Job deadline (schema downgrade unsupported) but the
	// operator must keep the Deployment on the prior image and surface MigrationFailed.
	It("OP-E2E-005b: Koku downgrade migration gates rollout (success or fail-closed)", func() {
		downgradeTag := os.Getenv("E2E_KOKU_DOWNGRADE_TAG")
		if downgradeTag == "" {
			downgradeTag = "5432d06"
		}

		cfg := getCMSC()
		originalTag := cfg.Spec.CostManagement.API.Image.Tag
		if originalTag == downgradeTag {
			Skip("downgrade tag must differ from current CMSC tag")
		}
		repo := cfg.Spec.CostManagement.API.Image.Repository
		depName := kokuAPIDeploymentName()
		originalImage := deploymentContainerImage(depName, "koku-api")

		defer restoreKokuImageTag(repo, originalTag)

		By("patching CMSC Koku API image tag (downgrade)")
		patchCMSCImageKoku(repo, downgradeTag)

		waitMigrationJobStarted(kokuMigrationJobName(), downgradeTag)
		assertRolloutBlockedDuringMigration(kokuMigrationJobName(), downgradeTag, depName, "koku-api", originalImage)

		By("waiting for migration Job to finish (success or failure)")
		Eventually(migrationJobTerminalForTag, cmscMigrationWait+cmscMigrationDeadline, 15*time.Second).
			WithArguments(kokuMigrationJobName(), downgradeTag).Should(BeTrue())

		if migrationJobCompleteForTag(kokuMigrationJobName(), downgradeTag) {
			By("downgrade migration succeeded — Deployment may roll to older image")
			waitCMSCCondition(costv1alpha1.ConditionSchemaUpToDate, string(metav1.ConditionTrue), cmscMigrationWait, "MigrationComplete")
			Eventually(func(g Gomega) {
				g.Expect(deploymentContainerImage(depName, "koku-api")).To(ContainSubstring(downgradeTag))
			}, cmscMigrationWait, 15*time.Second).Should(Succeed())
			waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), cmscMigrationWait)
			return
		}

		By("downgrade migration failed — operator must stay fail-closed on prior image")
		Expect(migrationJobFailedForTag(kokuMigrationJobName(), downgradeTag)).To(BeTrue())
		waitCMSCCondition(costv1alpha1.ConditionSchemaUpToDate, string(metav1.ConditionFalse), cmscMigrationWait, "MigrationFailed")
		waitCMSCCondition(costv1alpha1.ConditionDegraded, string(metav1.ConditionTrue), cmscMigrationWait, "MigrationFailed")
		Expect(deploymentContainerImage(depName, "koku-api")).To(Equal(originalImage))
		Expect(deploymentContainerImage(depName, "koku-api")).NotTo(ContainSubstring(downgradeTag))
	})

	// OP-E2E-006 — RBAC migrate Job sequencing.
	It("OP-E2E-006: RBAC migration before RBAC API rollout", func() {
		upgradeTag := os.Getenv("E2E_RBAC_UPGRADE_TAG")
		if upgradeTag == "" {
			Skip("set E2E_RBAC_UPGRADE_TAG to a newer lab image tag to run RBAC upgrade sequencing")
		}

		cfg := getCMSC()
		currentTag := cfg.Spec.RBAC.Image.Tag
		if currentTag == upgradeTag {
			Skip("E2E_RBAC_UPGRADE_TAG must differ from current CMSC tag")
		}
		repo := cfg.Spec.RBAC.Image.Repository
		depName := cmscName + "-rbac-api"
		if !deploymentExists(depName) {
			Skip("rbac-api Deployment not found")
		}
		oldImage := deploymentContainerImage(depName, "rbac-api")

		defer restoreRBACImageTag(repo, currentTag)

		By("patching CMSC RBAC image tag")
		patchCMSCImageRBAC(repo, upgradeTag)

		waitMigrationJobStarted(rbacMigrationJobName(), rbacMigrationJobImageTag(upgradeTag))
		assertRolloutBlockedDuringMigration(rbacMigrationJobName(), rbacMigrationJobImageTag(upgradeTag), depName, "rbac-api", oldImage)

		Eventually(migrationJobCompleteForTag, cmscMigrationWait, 15*time.Second).
			WithArguments(rbacMigrationJobName(), rbacMigrationJobImageTag(upgradeTag)).Should(BeTrue())

		Eventually(func(g Gomega) {
			g.Expect(deploymentContainerImage(depName, "rbac-api")).To(ContainSubstring(upgradeTag))
		}, cmscMigrationWait, 15*time.Second).Should(Succeed())
	})
})
