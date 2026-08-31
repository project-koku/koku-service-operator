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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

var _ = Describe("CMSC pause and resume", Ordered, Label("cmsc", "pause"), func() {
	const driftReplicas int32 = 3

	BeforeAll(func() {
		ensureCMSCNotPaused()
	})

	AfterAll(func() {
		ensureCMSCNotPaused()
	})

	// OP-E2E-001 — pause halts reconciliation (and OP-E2E-004 — no drift while paused).
	It("OP-E2E-001/004: pause halts drift correction", func() {
		depName := kokuAPIDeploymentName()
		if !deploymentExists(depName) {
			Skip("koku-api Deployment not found in namespace")
		}

		By("setting pause annotation on CMSC")
		setCMSCPaused(true)
		waitCMSCCondition(costv1alpha1.ConditionPaused, string(metav1.ConditionTrue), cmscDependencyWait, "AnnotationSet")

		By("scaling koku-api Deployment while paused")
		scaleDeployment(depName, driftReplicas)

		By("waiting less than drift window; manual scale should persist")
		Consistently(func(g Gomega) {
			g.Expect(deploymentReplicas(depName)).To(Equal(driftReplicas))
		}, cmscPauseSettleWait, 15*time.Second).Should(Succeed())

		prog := findCMSCCondition(getCMSC(), costv1alpha1.ConditionProgressing)
		Expect(prog).NotTo(BeNil())
		Expect(prog.Status).To(Equal(metav1.ConditionFalse))
		Expect(prog.Reason).To(Equal("Paused"))
	})

	// OP-E2E-002 — resume clears pause and drift corrects.
	It("OP-E2E-002: resume restores desired state", func() {
		depName := kokuAPIDeploymentName()
		desired := getCMSC().Spec.CostManagement.API.Replicas
		if desired == 0 {
			desired = 1
		}

		By("removing pause annotation")
		setCMSCPaused(false)
		waitCMSCCondition(costv1alpha1.ConditionPaused, string(metav1.ConditionFalse), cmscDriftWait, "Resumed")

		By("waiting for SSA drift correction (requeueDrift + buffer)")
		Eventually(func(g Gomega) {
			g.Expect(deploymentReplicas(depName)).To(Equal(desired))
		}, cmscDriftWait, 15*time.Second).Should(Succeed())

		waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), cmscMigrationWait)
	})
})
