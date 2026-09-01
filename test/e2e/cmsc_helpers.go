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
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/test/utils"
)

const (
	cmscPauseAnnotation = "costmanagementserviceconfigs.service.costmanagement.openshift.io/pause"
	cmscPauseValue      = "true"

	// requeueDrift in the reconciler is 5 minutes; add buffer for CI/lab jitter.
	cmscDriftWait         = 6 * time.Minute
	cmscMigrationWait     = 10 * time.Minute
	cmscMigrationDeadline = 10 * time.Minute // matches resources.MigrationDeadlineSeconds
	cmscDependencyWait    = 3 * time.Minute
	cmscPauseSettleWait   = 2 * time.Minute

	// Hostname with no endpoints — validation TCP probe fails quickly (BYOI path).
	cmscUnreachableHost = "cmsc-e2e-dep-unreachable.invalid"
)

var (
	cmscNamespace string
	cmscName      string
	cmscK8s       client.Client
	cmscCtx       context.Context
)

func cmscKubectl(args ...string) (string, error) {
	bin := os.Getenv("KUBECTL")
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.Command(bin, args...)
	return utils.Run(cmd)
}

func initCMSCTestEnv() {
	cmscCtx = context.Background()
	cmscNamespace = os.Getenv("NAMESPACE")
	if cmscNamespace == "" {
		cmscNamespace = "cost-onprem"
	}
	cmscName = os.Getenv("CMSC_NAME")
	if cmscName == "" {
		cmscName = os.Getenv("CR_NAME")
	}
	if cmscName == "" {
		cmscName = "cost-onprem"
	}

	Expect(costv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(batchv1.AddToScheme(scheme.Scheme)).To(Succeed())
	cfg, err := clientcmdOrInClusterConfig()
	Expect(err).NotTo(HaveOccurred(), "load kubeconfig for cluster e2e")

	cmscK8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	By("verifying CMSC CRD is installed")
	_, err = cmscKubectl("get", "crd", "costmanagementserviceconfigs.service.costmanagement.openshift.io")
	Expect(err).NotTo(HaveOccurred(), "CMSC CRD must exist (install operator first)")
}

func kokuAPIDeploymentName() string {
	return cmscName + "-koku-api"
}

func kokuMigrationJobName() string {
	return cmscName + "-koku-migrate"
}

func rbacMigrationJobName() string {
	return cmscName + "-rbac-migrate"
}

func databaseStatefulSetName() string {
	return cmscName + "-database"
}

func valkeyWorkloadName() string {
	return cmscName + "-valkey"
}

// bundledCacheWorkloadKind reports how the operator deploys bundled Valkey today.
// The reconciler uses a Deployment (see internal/resources/cache.go). When/if the
// operator adds a StatefulSet variant, set E2E_CACHE_WORKLOAD=statefulset to
// exercise that path.
func bundledCacheWorkloadKind() string {
	if k := os.Getenv("E2E_CACHE_WORKLOAD"); k != "" {
		return k
	}
	if statefulSetExists(valkeyWorkloadName()) {
		return "statefulset"
	}
	return "deployment"
}

func databaseBundledDeployed(cfg *costv1alpha1.CostManagementServiceConfig) bool {
	return costv1alpha1.BoolVal(cfg.Spec.Database.Deploy, true)
}

func cacheBundledDeployed(cfg *costv1alpha1.CostManagementServiceConfig) bool {
	return costv1alpha1.BoolVal(cfg.Spec.Cache.Deploy, true)
}

func boolPtr(v bool) *bool {
	return &v
}

func updateCMSC(mut func(*costv1alpha1.CostManagementServiceConfig)) {
	cfg := getCMSC()
	mut(cfg)
	Expect(cmscK8s.Update(cmscCtx, cfg)).To(Succeed())
}

func waitCMSCCondition(condType, status string, timeout time.Duration, allowedReasons ...string) {
	Eventually(func(g Gomega) {
		cond := findCMSCCondition(getCMSC(), condType)
		g.Expect(cond).NotTo(BeNil(), "condition %s should exist", condType)
		g.Expect(string(cond.Status)).To(Equal(status), "condition %s status", condType)
		if len(allowedReasons) > 0 {
			g.Expect(allowedReasons).To(ContainElement(cond.Reason),
				"condition %s reason (got %q)", condType, cond.Reason)
		}
	}, timeout, 10*time.Second).Should(Succeed())
}

func injectExternalDatabaseUnreachable() func() {
	cfg := getCMSC()
	saved := cfg.Spec.Database
	port := saved.Port
	if port == 0 {
		port = 5432
	}
	secretName := saved.SecretName
	if secretName == "" {
		secretName = cmscName + "-db-credentials"
	}
	updateCMSC(func(c *costv1alpha1.CostManagementServiceConfig) {
		c.Spec.Database.Deploy = boolPtr(false)
		c.Spec.Database.Host = cmscUnreachableHost
		c.Spec.Database.Port = port
		c.Spec.Database.SecretName = secretName
	})
	return func() {
		updateCMSC(func(c *costv1alpha1.CostManagementServiceConfig) {
			c.Spec.Database = saved
		})
	}
}

func ensureCacheCredentialsSecret(name string) bool {
	secret := &corev1.Secret{}
	err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: name}, secret)
	if err == nil {
		return false
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cmscNamespace,
		},
		StringData: map[string]string{
			"redis-password": "e2e-placeholder",
		},
	}
	Expect(cmscK8s.Create(cmscCtx, secret)).To(Succeed())
	return true
}

func injectExternalCacheUnreachable() func() {
	cfg := getCMSC()
	saved := cfg.Spec.Cache
	port := saved.Port
	if port == 0 {
		port = 6379
	}
	secretName := saved.Auth.SecretName
	createdSecret := false
	if secretName == "" {
		secretName = cmscName + "-cache-credentials"
		createdSecret = ensureCacheCredentialsSecret(secretName)
	}
	updateCMSC(func(c *costv1alpha1.CostManagementServiceConfig) {
		c.Spec.Cache.Deploy = boolPtr(false)
		c.Spec.Cache.Host = cmscUnreachableHost
		c.Spec.Cache.Port = port
		c.Spec.Cache.Auth.SecretName = secretName
	})
	return func() {
		updateCMSC(func(c *costv1alpha1.CostManagementServiceConfig) {
			c.Spec.Cache = saved
		})
		if createdSecret {
			Expect(client.IgnoreNotFound(cmscK8s.Delete(cmscCtx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: cmscNamespace},
			}))).To(Succeed())
		}
	}
}

func deleteWorkloadPods(kind, name string) {
	var selector map[string]string
	switch kind {
	case "statefulset":
		sts := &appsv1.StatefulSet{}
		err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: name}, sts)
		Expect(err).NotTo(HaveOccurred(), "get StatefulSet %s", name)
		selector = sts.Spec.Selector.MatchLabels
	case "deployment":
		dep := getDeployment(name)
		selector = dep.Spec.Selector.MatchLabels
	default:
		Fail(fmt.Sprintf("unsupported workload kind %q", kind))
	}
	sel := labels.SelectorFromSet(selector)
	pods := &corev1.PodList{}
	Expect(cmscK8s.List(cmscCtx, pods,
		client.InNamespace(cmscNamespace),
		client.MatchingLabelsSelector{Selector: sel},
	)).To(Succeed())
	for i := range pods.Items {
		pod := &pods.Items[i]
		Expect(cmscK8s.Delete(cmscCtx, pod)).To(Succeed())
	}
}

func bundledWorkloadExists(kind, name string) bool {
	switch kind {
	case "statefulset":
		return statefulSetExists(name)
	case "deployment":
		return deploymentExists(name)
	default:
		return false
	}
}

func getCMSC() *costv1alpha1.CostManagementServiceConfig {
	cfg := &costv1alpha1.CostManagementServiceConfig{}
	err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: cmscName}, cfg)
	Expect(err).NotTo(HaveOccurred(), "get CMSC %s/%s", cmscNamespace, cmscName)
	return cfg
}

func findCMSCCondition(cfg *costv1alpha1.CostManagementServiceConfig, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(cfg.Status.Conditions, condType)
}

func waitCMSCHealthyGate() {
	By("waiting for day-one CMSC gate (SchemaUpToDate=True, Available=True)")
	waitCMSCCondition(costv1alpha1.ConditionSchemaUpToDate, string(metav1.ConditionTrue), cmscMigrationWait)
	waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), cmscMigrationWait)
}

func waitCMSCBlockingDependencyFailure(timeout time.Duration) {
	waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionFalse), timeout, "DependencyNotReady")
	waitCMSCCondition(costv1alpha1.ConditionDegraded, string(metav1.ConditionTrue), timeout, "DependencyUnreachable")
}

func waitCMSCHealthyAfterDependencyRestore(timeout time.Duration) {
	waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), timeout)
	waitCMSCCondition(costv1alpha1.ConditionDegraded, string(metav1.ConditionFalse), timeout)
}

func ensureCMSCNotPaused() {
	cfg := getCMSC()
	if cfg.Annotations == nil {
		return
	}
	if _, ok := cfg.Annotations[cmscPauseAnnotation]; !ok {
		return
	}
	delete(cfg.Annotations, cmscPauseAnnotation)
	Expect(cmscK8s.Update(cmscCtx, cfg)).To(Succeed())
	waitCMSCCondition(costv1alpha1.ConditionPaused, string(metav1.ConditionFalse), cmscDriftWait, "Resumed")
}

func setCMSCPaused(paused bool) {
	cfg := getCMSC()
	if cfg.Annotations == nil {
		cfg.Annotations = map[string]string{}
	}
	if paused {
		cfg.Annotations[cmscPauseAnnotation] = cmscPauseValue
	} else {
		delete(cfg.Annotations, cmscPauseAnnotation)
	}
	Expect(cmscK8s.Update(cmscCtx, cfg)).To(Succeed())
}

func getDeployment(name string) *appsv1.Deployment {
	dep := &appsv1.Deployment{}
	err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: name}, dep)
	Expect(err).NotTo(HaveOccurred(), "get Deployment %s", name)
	return dep
}

func deploymentExists(name string) bool {
	dep := &appsv1.Deployment{}
	err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: name}, dep)
	return err == nil
}

func statefulSetExists(name string) bool {
	sts := &appsv1.StatefulSet{}
	err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: name}, sts)
	return err == nil
}

func deploymentReplicas(name string) int32 {
	dep := getDeployment(name)
	if dep.Spec.Replicas == nil {
		return 1
	}
	return *dep.Spec.Replicas
}

func scaleDeployment(name string, replicas int32) {
	dep := getDeployment(name)
	dep.Spec.Replicas = &replicas
	Expect(cmscK8s.Update(cmscCtx, dep)).To(Succeed())
}

func deploymentContainerImage(name, container string) string {
	dep := getDeployment(name)
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == container {
			return c.Image
		}
	}
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		return dep.Spec.Template.Spec.Containers[0].Image
	}
	return ""
}

func getMigrationJob(jobName string) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := cmscK8s.Get(cmscCtx, types.NamespacedName{Namespace: cmscNamespace, Name: jobName}, job)
	return job, err
}

func migrationJobActive(jobName string) bool {
	job, err := getMigrationJob(jobName)
	if err != nil {
		return false
	}
	return job.Status.Active > 0
}

func migrationJobComplete(jobName string) bool {
	job, err := getMigrationJob(jobName)
	if err != nil {
		return false
	}
	return job.Status.Succeeded > 0
}

func migrationJobFailed(jobName string) bool {
	job, err := getMigrationJob(jobName)
	if err != nil {
		return false
	}
	return job.Status.Failed > 0
}

func migrationJobTerminal(jobName string) bool {
	return migrationJobComplete(jobName) || migrationJobFailed(jobName)
}

func waitMigrationJobStarted(jobName string) {
	Eventually(func(g Gomega) {
		g.Expect(migrationJobActive(jobName) || migrationJobTerminal(jobName)).
			To(BeTrue(), "expected migration Job %s to start", jobName)
	}, cmscDependencyWait, 10*time.Second).Should(Succeed())
}

// assertRolloutBlockedDuringMigration waits for the migration Job to finish while
// asserting the Deployment image stays on wantImage whenever the Job is active.
func assertRolloutBlockedDuringMigration(jobName, depName, container, wantImage string) {
	By("verifying Deployment does not roll while migration Job is active")
	Eventually(func(g Gomega) {
		if migrationJobActive(jobName) {
			g.Expect(deploymentContainerImage(depName, container)).To(Equal(wantImage),
				"Deployment must not roll while migration Job %s is running", jobName)
			cond := findCMSCCondition(getCMSC(), costv1alpha1.ConditionSchemaUpToDate)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("MigrationRunning"))
		}
		g.Expect(migrationJobTerminal(jobName)).To(BeTrue(),
			"migration Job %s should reach a terminal state", jobName)
	}, cmscMigrationWait+cmscMigrationDeadline, 2*time.Second).Should(Succeed())
}

func restoreKokuImageTag(repo, tag string) {
	patchCMSCImageKoku(repo, tag)
	waitCMSCCondition(costv1alpha1.ConditionSchemaUpToDate, string(metav1.ConditionTrue), cmscMigrationWait)
	waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), cmscMigrationWait)
}

func restoreRBACImageTag(repo, tag string) {
	patchCMSCImageRBAC(repo, tag)
	waitCMSCCondition(costv1alpha1.ConditionSchemaUpToDate, string(metav1.ConditionTrue), cmscMigrationWait)
	waitCMSCCondition(costv1alpha1.ConditionAvailable, string(metav1.ConditionTrue), cmscMigrationWait)
}

func patchCMSCImageKoku(repository, tag string) {
	patch := fmt.Sprintf(
		`{"spec":{"costManagement":{"api":{"image":{"repository":%q,"tag":%q}}}}}`,
		repository, tag,
	)
	patchCMSCJSON(patch)
}

func patchCMSCImageRBAC(repository, tag string) {
	patch := fmt.Sprintf(
		`{"spec":{"rbac":{"image":{"repository":%q,"tag":%q}}}}`,
		repository, tag,
	)
	patchCMSCJSON(patch)
}

func patchCMSCJSON(patch string) {
	_, err := cmscKubectl(
		"patch", "cmsc", cmscName,
		"-n", cmscNamespace,
		"--type", "merge",
		"-p", patch,
	)
	Expect(err).NotTo(HaveOccurred(), "patch CMSC: %s", patch)
}

func collectCMSCForensics() {
	_, _ = fmt.Fprintln(GinkgoWriter, "=== CMSC e2e forensics ===")

	if out, err := cmscKubectl("get", "cmsc", cmscName, "-n", cmscNamespace, "-o", "yaml"); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- CMSC status ---\n%s\n", out)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "CMSC fetch failed: %v\n", err)
	}

	if out, err := cmscKubectl(
		"get", "events", "-n", cmscNamespace,
		"--field-selector", fmt.Sprintf("involvedObject.name=%s", cmscName),
		"--sort-by=.lastTimestamp",
	); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- CMSC events ---\n%s\n", out)
	}

	if out, err := collectOperatorLogs(); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "--- operator logs (tail 200) ---\n%s\n", out)
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "operator logs failed: %v\n", err)
	}
}

func collectOperatorLogs() (string, error) {
	for _, name := range []string{
		"koku-service-operator-controller-manager", // OLM CSV deployment name
		"koku-service-operator",                    // deploy-incluster.sh
	} {
		if out, err := cmscKubectl("logs", "deploy/"+name, "-n", cmscNamespace, "--tail=200"); err == nil {
			return out, nil
		}
	}
	return cmscKubectl(
		"logs", "-l", "control-plane=controller-manager",
		"-n", cmscNamespace, "--tail=200",
	)
}
