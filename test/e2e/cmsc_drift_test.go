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

//go:build cluster_e2e

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

var _ = Describe("CMSC drift correction", Label("cmsc", "drift"), func() {
	BeforeEach(func() {
		ensureCMSCNotPaused()
	})

	// OP-E2E-003 — SSA reverts manual Deployment edits after requeueDrift.
	It("OP-E2E-003: reverts manual Deployment scale", func() {
		depName := kokuAPIDeploymentName()
		if !deploymentExists(depName) {
			Skip("koku-api Deployment not found in namespace")
		}

		desired := getCMSC().Spec.CostManagement.API.Replicas
		if desired == 0 {
			desired = 1
		}
		driftReplicas := desired + 2

		By("recording desired replica count from CMSC spec")
		Expect(deploymentReplicas(depName)).To(Equal(desired), "stack should match CMSC before drift test")

		By("manually scaling Deployment away from desired state")
		scaleDeployment(depName, driftReplicas)
		Expect(deploymentReplicas(depName)).To(Equal(driftReplicas))

		By("waiting for periodic SSA drift correction")
		Eventually(func(g Gomega) {
			g.Expect(deploymentReplicas(depName)).To(Equal(desired))
		}, cmscDriftWait, 15*time.Second).Should(Succeed())

		waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), "", cmscMigrationWait)
	})
})
