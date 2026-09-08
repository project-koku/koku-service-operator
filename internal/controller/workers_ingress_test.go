package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

func TestReconcileWorkers_IngressNotReady(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	r := &CostManagementServiceConfigReconciler{
		Client:   fakeClientWithApplySupport(scheme),
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}
	result, err := r.reconcileWorkers(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reconcileWorkers: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while Ingress Deployment is not ready")
	}
	mustExist(t, r.Client, testNamespace, resources.NameIngress(cfg), &appsv1.Deployment{})
	cond := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionIngressReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WaitingForIngress" {
		t.Fatalf("expected IngressReady=False WaitingForIngress, got %+v", cond)
	}
}

func TestReconcileWorkers_IngressReady(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}
	if _, err := r.reconcileWorkers(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameIngress(cfg))
	result, err := r.reconcileWorkers(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("expected zero result once Ingress is ready, got %+v", result)
	}
	if !apimeta.IsStatusConditionTrue(cfg.Status.Conditions, costv1alpha1.ConditionIngressReady) {
		t.Fatal("expected IngressReady=True")
	}
}

func TestReconcileWorkers_ROSAPINotReady_BlocksProgress(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.ROS.Enabled = new(true)
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}
	if _, err := r.reconcileWorkers(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameIngress(cfg))

	result, err := r.reconcileWorkers(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while ROS API is not ready")
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != reasonWaitingForROSAPI {
		t.Fatalf("expected Available=False %s, got %+v", reasonWaitingForROSAPI, avail)
	}
}

func TestReconcileWorkers_ROSProcessorNotReady_BlocksProgress(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.ROS.Enabled = new(true)
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}
	if _, err := r.reconcileWorkers(context.Background(), cfg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameIngress(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameROSAPI(cfg))

	result, err := r.reconcileWorkers(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while ROS Processor is not ready")
	}
	avail := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != reasonWaitingForROSProcessor {
		t.Fatalf("expected Available=False %s, got %+v", reasonWaitingForROSProcessor, avail)
	}
}

// Core must not promote Available=True between worker wait passes, or the
// 5-minute DeploymentNotReady clock for ROS API never elapses.
func TestReconcileCoreThenWorkers_ROSAPINotReady_PreservesWaitClock(t *testing.T) {
	scheme := ownershipScheme(t)
	cfg := minimalCR(testCRName, testNamespace)
	cfg.Spec.ROS.Enabled = new(true)
	c := fakeClientPreservingStatus(scheme)
	r := &CostManagementServiceConfigReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: &noopRecorder{},
	}

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core first pass: %v", err)
	}
	if _, err := r.reconcileWorkers(context.Background(), cfg); err != nil {
		t.Fatalf("workers first pass: %v", err)
	}
	markDeploymentReady(t, c, testNamespace, resources.NameRBACAPI(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameRBACWorker(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameKokuAPI(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameMasu(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameListener(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameKruize(cfg))
	markDeploymentReady(t, c, testNamespace, resources.NameIngress(cfg))

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core ready pass: %v", err)
	}
	if _, err := r.reconcileWorkers(context.Background(), cfg); err != nil {
		t.Fatalf("workers ROS wait pass: %v", err)
	}
	first := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if first == nil || first.Status != metav1.ConditionFalse || first.Reason != reasonWaitingForROSAPI {
		t.Fatalf("expected Available=False %s, got %+v", reasonWaitingForROSAPI, first)
	}
	started := first.LastTransitionTime

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core second pass: %v", err)
	}
	if _, err := r.reconcileWorkers(context.Background(), cfg); err != nil {
		t.Fatalf("workers second pass: %v", err)
	}
	second := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	if second == nil || second.Status != metav1.ConditionFalse || second.Reason != reasonWaitingForROSAPI {
		t.Fatalf("expected Available=False %s after core re-ran, got %+v", reasonWaitingForROSAPI, second)
	}
	if !second.LastTransitionTime.Equal(&started) {
		t.Fatalf("ROS wait clock reset: %v -> %v", started, second.LastTransitionTime)
	}

	apimeta.RemoveStatusCondition(&cfg.Status.Conditions, costv1alpha1.ConditionAvailable)
	apimeta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               costv1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             reasonWaitingForROSAPI,
		Message:            msgWaitingForROSAPI,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-6 * time.Minute)),
	})

	if _, err := r.reconcileCoreServices(context.Background(), cfg); err != nil {
		t.Fatalf("core timeout pass: %v", err)
	}
	result, err := r.reconcileWorkers(context.Background(), cfg)
	if err != nil {
		t.Fatalf("workers timeout pass: %v", err)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Fatalf("expected 2m backoff after a 6m ROS wait, got %s", result.RequeueAfter)
	}
	deg := findCondition(cfg.Status.Conditions, costv1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != reasonDeploymentNotReady {
		t.Fatalf("expected Degraded=True %s, got %+v", reasonDeploymentNotReady, deg)
	}
	if deg.Message != "ROS API is not ready" {
		t.Fatalf("expected Degraded message to name ROS API, got %+v", deg)
	}
}
