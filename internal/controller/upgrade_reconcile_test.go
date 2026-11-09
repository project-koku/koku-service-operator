package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// Exercise the production Reconcile entry point and phase order with persisted
// CMSC status. Only external platform controllers and API failures are simulated.
type upgradeReconcileFixture struct {
	t                *testing.T
	r                *CostManagementServiceConfigReconciler
	c                client.Client
	cfg              *cost.CostManagementServiceConfig
	rejectUI         bool
	sourcePatchError bool
}

func newUpgradeReconcileFixture(t *testing.T) *upgradeReconcileFixture {
	r, cfg, seed := lifecyclePlatform(t)
	secrets := &corev1.SecretList{}
	if err := seed.List(t.Context(), secrets); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, 1+len(secrets.Items))
	objs = append(objs, cfg)
	for i := range secrets.Items {
		objs = append(objs, &secrets.Items[i])
	}
	c := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(objs...).
		WithStatusSubresource(cfg, &appsv1.Deployment{}, &appsv1.StatefulSet{}, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return applyAsCreateOrUpdate(ctx, inner, obj)
		}}).Build()
	lab := &upgradeReconcileFixture{t: t, r: r, c: c, cfg: cfg}
	r.Client = interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, inner client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if d, ok := obj.(*appsv1.Deployment); ok {
			if lab.sourcePatchError && d.Name == resources.NameKokuAPI(lab.cfg) {
				return errors.New("injected transient API patch failure")
			}
			if lab.rejectUI && d.Name == resources.NameUI(lab.cfg) {
				for _, container := range d.Spec.Template.Spec.Containers {
					if strings.Contains(container.Image, ":bad-ui") {
						return errors.New("injected UI deployment patch rejection")
					}
				}
			}
		}
		return inner.Patch(ctx, obj, p, opts...)
	}})
	r.APIReader = c
	lab.drive(func() bool { return lab.cfg.Status.Upgrade.Installed != nil && lab.cfg.Status.Phase == cost.PhaseReady })
	drainEvents(r)
	return lab
}
func (l *upgradeReconcileFixture) read() {
	l.t.Helper()
	if err := l.c.Get(l.t.Context(), client.ObjectKeyFromObject(l.cfg), l.cfg); err != nil {
		l.t.Fatal(err)
	}
}
func (l *upgradeReconcileFixture) step() error {
	l.t.Helper()
	_, err := l.r.Reconcile(l.t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(l.cfg)})
	l.read()
	return err
}
func (l *upgradeReconcileFixture) edit(f func()) {
	l.t.Helper()
	l.read()
	f()
	l.cfg.Generation++
	if err := l.c.Update(l.t.Context(), l.cfg); err != nil {
		l.t.Fatal(err)
	}
	l.read()
}
func (l *upgradeReconcileFixture) status(f func()) {
	l.t.Helper()
	l.read()
	f()
	if err := l.c.Status().Update(l.t.Context(), l.cfg); err != nil {
		l.t.Fatal(err)
	}
	l.read()
}
func (l *upgradeReconcileFixture) acknowledge() {
	l.t.Helper()
	deps := &appsv1.DeploymentList{}
	if err := l.c.List(l.t.Context(), deps); err != nil {
		l.t.Fatal(err)
	}
	for _, d := range deps.Items {
		markDeploymentReady(l.t, l.c, l.cfg.Namespace, d.Name)
	}
	states := &appsv1.StatefulSetList{}
	if err := l.c.List(l.t.Context(), states); err != nil {
		l.t.Fatal(err)
	}
	for _, s := range states.Items {
		markStatefulSetReady(l.t, l.c, l.cfg.Namespace, s.Name)
	}
	jobs := &batchv1.JobList{}
	if err := l.c.List(l.t.Context(), jobs); err != nil {
		l.t.Fatal(err)
	}
	for _, j := range jobs.Items {
		if !isJobComplete(&j) && !isJobFailed(&j) {
			markJobComplete(l.t, l.c, l.cfg.Namespace, j.Name)
		}
	}
	for _, name := range []string{resources.NameAPIRoute(l.cfg), resources.NameUI(l.cfg)} {
		if _, err := l.r.getRoute(l.t.Context(), l.cfg.Namespace, name); err == nil {
			markRouteAdmitted(l.t, l.c, l.cfg.Namespace, name)
		}
	}
}
func (l *upgradeReconcileFixture) drive(done func() bool) {
	l.t.Helper()
	for range 60 {
		if err := l.step(); err != nil {
			l.t.Fatal(err)
		}
		l.acknowledge()
		if done() {
			return
		}
	}
	l.t.Fatalf("did not settle: phase=%s operation=%+v conditions=%+v", l.cfg.Status.Phase, l.cfg.Status.Upgrade.Operation, l.cfg.Status.Conditions)
}
func (l *upgradeReconcileFixture) dep(name string) *appsv1.Deployment {
	l.t.Helper()
	d := &appsv1.Deployment{}
	if err := l.c.Get(l.t.Context(), client.ObjectKey{Namespace: l.cfg.Namespace, Name: name}, d); err != nil {
		l.t.Fatal(err)
	}
	return d
}
func (l *upgradeReconcileFixture) failedUI() {
	l.rejectUI = true
	l.edit(func() { l.cfg.Spec.UI.App.Image.Tag = "bad-ui-1" })
	l.drive(func() bool {
		op := l.cfg.Status.Upgrade.Operation
		return op != nil && op.Stage == cost.UpgradeStageRolledBack
	})
	if l.cfg.Status.Upgrade.Operation.SchemaMutationPossible {
		l.t.Fatal("UI-only failure unexpectedly changed schema risk")
	}
	if *l.dep(resources.NameKokuAPI(l.cfg)).Spec.Replicas == 0 {
		l.t.Fatal("initial UI failure drained API")
	}
	drainEvents(l.r)
}

func TestRejectedTargetPreservesFailureAcrossReconciles(t *testing.T) {
	l := newUpgradeReconcileFixture(t)
	// Valid CMSC edit: unpin Koku while the custom RBAC pin remains. This
	// fails the release-transition policy, not admission or a test-only helper.
	l.edit(func() { l.cfg.Spec.CostManagement.API.Image = cost.ImageSpec{} })
	if err := l.step(); err == nil || !strings.Contains(err.Error(), "mix custom pins") {
		t.Fatalf("expected transition rejection, got %v", err)
	}
	since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	l.status(func() {
		apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionDegraded).LastTransitionTime = since
	})
	drainEvents(l.r)
	for range 2 {
		if err := l.step(); err == nil {
			t.Fatal("expected repeat rejection")
		}
	}
	events := drainEvents(l.r)
	degraded := apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionDegraded)
	if countEvents(events, "Ready") != 0 || countEvents(events, "PhaseChanged") != 0 || !degraded.LastTransitionTime.Equal(&since) {
		t.Fatalf("rejected target announced success or reset failure: events=%v condition=%+v", events, degraded)
	}
	if l.cfg.Status.Phase != cost.PhaseDegraded || l.cfg.Status.Upgrade.Operation != nil {
		t.Fatal("unexpected final state")
	}
}

func TestRecoveryHoldPreservesProgressingTransition(t *testing.T) {
	for _, invalidTarget := range []bool{false, true} {
		name := "failed_target"
		if invalidTarget {
			name = "invalid_target"
		}
		t.Run(name, func(t *testing.T) {
			l := newUpgradeReconcileFixture(t)
			l.status(func() {
				attachUpgrade(l.cfg, cost.UpgradeStageRollingBack, true)
				if invalidTarget {
					l.cfg.Status.Upgrade.Operation.Target.Spec.Raw = []byte(`"oops"`)
				}
			})
			l.drive(func() bool {
				return l.cfg.Status.Upgrade.Operation.Stage == cost.UpgradeStageRecoveryRequired
			})
			since := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
			l.status(func() {
				apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionProgressing).LastTransitionTime = since
			})
			before := apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionProgressing).DeepCopy()
			for range 2 {
				if err := l.step(); err != nil {
					t.Fatal(err)
				}
				got := apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionProgressing)
				if got.Status != before.Status || got.Reason != before.Reason || !got.LastTransitionTime.Equal(&since) {
					t.Fatalf("stable recovery hold changed Progressing: before=%+v after=%+v", before, got)
				}
			}
		})
	}
}

func TestRolledBackHoldContinuesDriftCorrection(t *testing.T) {
	for _, tc := range []struct {
		name             string
		aged, patchError bool
	}{
		{"recent_error", false, true}, {"aged_error", true, true},
		{"recent_readiness_wait", false, false}, {"aged_readiness_wait", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newUpgradeReconcileFixture(t)
			l.failedUI()
			if tc.aged {
				l.status(func() { l.cfg.Status.Upgrade.Operation.StageStartedAt = metav1.NewTime(time.Now().Add(-3 * time.Hour)) })
			}
			l.sourcePatchError = tc.patchError
			d := l.dep(resources.NameKokuAPI(l.cfg))
			if !tc.patchError {
				d.Status.AvailableReplicas = 0
				if err := l.c.Status().Update(t.Context(), d); err != nil {
					t.Fatal(err)
				}
			}
			err := l.step()
			if (err != nil) != tc.patchError {
				t.Fatalf("unexpected injected result: %v", err)
			}
			if l.cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageRolledBack {
				t.Fatal("completed rollback entered recovery")
			}
			l.sourcePatchError = false
			d = l.dep(resources.NameKokuAPI(l.cfg))
			d.Spec.Replicas = new(int32(0))
			if err := l.c.Update(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := l.step(); err != nil {
					t.Fatal(err)
				}
				l.acknowledge()
			}
			if *l.dep(resources.NameKokuAPI(l.cfg)).Spec.Replicas == 0 {
				t.Fatal("source drift correction stopped")
			}
			if !apimeta.IsStatusConditionTrue(l.cfg.Status.Conditions, cost.ConditionAvailable) || !apimeta.IsStatusConditionTrue(l.cfg.Status.Conditions, cost.ConditionUpgradeFailed) {
				t.Fatal("source availability and failed target not preserved")
			}
		})
	}
}

func TestFailedImageOnlyRetryPreservesBackend(t *testing.T) {
	l := newUpgradeReconcileFixture(t)
	l.failedUI()
	before := l.cfg.Status.Upgrade.Installed.DeepCopy()
	l.edit(func() {
		l.cfg.Spec.UI.App.Image.Tag = "bad-ui-2"
		l.cfg.Spec.Upgrade.Recovery = &cost.UpgradeRecoveryRequest{OperationID: l.cfg.Status.Upgrade.Operation.ID, RequestID: "retry-second-ui", Action: cost.RecoveryRetry}
	})
	if err := l.step(); err != nil {
		t.Fatal(err)
	}
	op := l.cfg.Status.Upgrade.Operation
	if op.RecoveryAction != cost.RecoveryRetry || !op.RecoveryImageOnly || op.Stage != cost.UpgradeStageRollingOut || op.SchemaMutationPossible || !reflect.DeepEqual(op.Source.Migrations, op.Target.Migrations) {
		t.Fatalf("not image-only retry: %+v", op)
	}
	for name, ref := range before.Images {
		if name != "ui" && op.Target.Images[name] != ref {
			t.Fatalf("unexpected changed image %s", name)
		}
	}
	// A spec edit after acceptance must not rewrite the action in the journal.
	l.edit(func() { l.cfg.Spec.Upgrade.Recovery.Action = cost.RecoveryResumePrevious })
	for pass := range 30 {
		if err := l.step(); err != nil {
			t.Fatal(err)
		}
		if *l.dep(resources.NameKokuAPI(l.cfg)).Spec.Replicas == 0 {
			t.Fatal("failed UI-only Retry drained the unchanged API")
		}
		l.acknowledge()
		if l.cfg.Status.Upgrade.Operation.Stage == cost.UpgradeStageRolledBack {
			return
		}
		if pass == 29 {
			t.Fatal("image-only retry did not restore source")
		}
	}

}

func TestRetryTimeoutPreservesDrainBarrier(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminating Pod", true: "active CronJob child"}[scheduled], func(t *testing.T) {
			l := newUpgradeReconcileFixture(t)
			if scheduled {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sync-client", Namespace: l.cfg.Namespace}, Data: map[string][]byte{"client-secret": []byte("test")}}
				if err := l.c.Create(t.Context(), secret); err != nil {
					t.Fatal(err)
				}
				l.edit(func() {
					l.cfg.Spec.RBAC.KeycloakSync.Enabled = true
					l.cfg.Spec.RBAC.KeycloakSync.ClientSecretRef.Name = secret.Name
				})
				l.drive(func() bool { return l.cfg.Status.Phase == cost.PhaseReady && l.cfg.Status.Upgrade.Operation == nil })
			}
			l.failedUI()
			var finishWriter func()
			if scheduled {
				cron := resources.KeycloakSyncCronJob(l.cfg)
				if err := l.c.Get(t.Context(), client.ObjectKeyFromObject(cron), cron); err != nil {
					t.Fatal(err)
				}
				cron.UID = "active-schedule"
				cron.Spec.Suspend = new(true)
				if err := l.c.Update(t.Context(), cron); err != nil {
					t.Fatal(err)
				}
				// CronJob children need not carry the operator's application labels.
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "still-running", Namespace: l.cfg.Namespace,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: cronJobKind, Name: cron.Name, UID: cron.UID, Controller: new(true)}}}}
				if err := l.c.Create(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				finishWriter = func() { markJobComplete(t, l.c, l.cfg.Namespace, job.Name) }
			} else {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "still-draining", Namespace: l.cfg.Namespace,
					Labels: resources.Labels(l.cfg, "listener"), Finalizers: []string{"test-drain"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if err := l.c.Create(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
				if err := l.c.Delete(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
				finishWriter = func() {
					if err := l.c.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
						t.Fatal(err)
					}
					pod.Finalizers = nil
					if err := l.c.Update(t.Context(), pod); err != nil {
						t.Fatal(err)
					}
				}
			}
			l.edit(func() {
				l.cfg.Spec.CostManagement.API.Image.Tag = "retry-backend"
				l.cfg.Spec.Upgrade.Recovery = &cost.UpgradeRecoveryRequest{OperationID: l.cfg.Status.Upgrade.Operation.ID, RequestID: "backend-retry", Action: cost.RecoveryRetry}
			})
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			op := l.cfg.Status.Upgrade.Operation
			if op.Stage != cost.UpgradeStageRecovering || op.RecoveryImageOnly || op.SchemaMutationPossible || reflect.DeepEqual(op.Source.Migrations, op.Target.Migrations) {
				t.Fatalf("expected Retry requiring migrations before schema mutation: %+v", op)
			}
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			for _, d := range resources.ApplicationDeployments(l.cfg) {
				markDeploymentReady(t, l.c, l.cfg.Namespace, d.Name)
			}
			if *l.dep(resources.NameKokuAPI(l.cfg)).Spec.Replicas != 0 {
				t.Fatal("Retry did not start draining applications")
			}
			l.status(func() { l.cfg.Status.Upgrade.Operation.StartedAt = metav1.NewTime(time.Now().Add(-3 * time.Hour)) })
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			if l.cfg.Status.Upgrade.Operation.Stage != cost.UpgradeStageRollingBack {
				t.Fatal("Retry deadline did not enter rollback")
			}
			for range 3 {
				if err := l.step(); err != nil {
					t.Fatal(err)
				}
				if *l.dep(resources.NameKokuAPI(l.cfg)).Spec.Replicas != 0 {
					t.Fatal("Retry timeout restarted source workloads before the writer stopped")
				}
			}
			finishWriter()
			l.drive(func() bool { return l.cfg.Status.Upgrade.Operation.Stage == cost.UpgradeStageRolledBack })
			if *l.dep(resources.NameKokuAPI(l.cfg)).Spec.Replicas == 0 {
				t.Fatal("source did not resume after writer drain")
			}
		})
	}
}

func TestListenerDrainBudgetThroughReconcile(t *testing.T) {
	for _, journaled := range []bool{false, true} {
		t.Run(map[bool]string{false: "runtime_env_change", true: "control_journaled_upgrade"}[journaled], func(t *testing.T) {
			l := newUpgradeReconcileFixture(t)
			original := l.cfg.Status.Upgrade.Installed.DeepCopy()
			l.edit(func() {
				if journaled {
					l.cfg.Spec.UI.App.Image.Tag = "next-ui"
				} else {
					l.cfg.Spec.CostManagement.Listener.Env = map[string]string{"LOG_LEVEL": "DEBUG"}
				}
			})
			d := l.dep(resources.NameListener(l.cfg))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-listener", Namespace: l.cfg.Namespace, Labels: d.Spec.Selector.MatchLabels, Finalizers: []string{"test-drain"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			if err := l.c.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if err := l.c.Delete(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			d.Status.UpdatedReplicas = 0
			d.Status.ReadyReplicas = 0
			d.Status.AvailableReplicas = 0
			if err := l.c.Status().Update(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			if journaled {
				if err := l.step(); err != nil {
					t.Fatal(err)
				}
			}
			cond := apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionAvailable)
			if cond == nil || cond.Reason != reasonWaitingForOldPods {
				t.Fatalf("not at listener gate: %+v", cond)
			}
			l.status(func() {
				apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionAvailable).LastTransitionTime = metav1.NewTime(time.Now().Add(-10 * time.Minute))
			})
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			degraded := apimeta.IsStatusConditionTrue(l.cfg.Status.Conditions, cost.ConditionDegraded)
			if degraded {
				t.Fatalf("unexpected degradation=%v journaled=%v", degraded, journaled)
			}
			if !journaled && (l.cfg.Status.Upgrade.Operation != nil || l.cfg.Status.Upgrade.Installed.ID != original.ID) {
				t.Fatal("runtime change unexpectedly journaled")
			}
			if err := l.c.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
				t.Fatal(err)
			}
			if pod.DeletionTimestamp.IsZero() {
				t.Fatal("old pod not terminating")
			}
			// Ordinary drains still have a finite budget.
			if !journaled {
				l.status(func() {
					apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionAvailable).LastTransitionTime = metav1.NewTime(time.Now().Add(-70 * time.Minute))
				})
				if err := l.step(); err != nil {
					t.Fatal(err)
				}
				if !apimeta.IsStatusConditionTrue(l.cfg.Status.Conditions, cost.ConditionDegraded) {
					t.Fatal("expired listener drain did not report Degraded")
				}
			}
			// After the old Pod exits, a broken new Pod gets the ordinary timeout.
			pod.Finalizers = nil
			if err := l.c.Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			cond = apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionAvailable)
			if cond.Reason != reasonWaitingForListener || time.Since(cond.LastTransitionTime.Time) > time.Minute {
				t.Fatalf("new Pod did not get a fresh readiness wait: %+v", cond)
			}
			l.status(func() {
				apimeta.FindStatusCondition(l.cfg.Status.Conditions, cost.ConditionAvailable).LastTransitionTime = metav1.NewTime(time.Now().Add(-10 * time.Minute))
			})
			if err := l.step(); err != nil {
				t.Fatal(err)
			}
			if apimeta.IsStatusConditionTrue(l.cfg.Status.Conditions, cost.ConditionDegraded) == journaled {
				t.Fatal("ordinary readiness and operation deadlines were not kept separate")
			}
			l.acknowledge()
			l.drive(func() bool { return l.cfg.Status.Phase == cost.PhaseReady && l.cfg.Status.Upgrade.Operation == nil })
		})
	}
}

func TestListenerReadinessDistinguishesDrainFromUnhealthyPod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		phase       corev1.PodPhase
		terminating bool
		listError   bool
		wantDrain   bool
	}{
		{name: "serving old Pod", phase: corev1.PodRunning},
		{name: "unready new Pod", phase: corev1.PodPending},
		{name: "completed terminating Pod", phase: corev1.PodSucceeded, terminating: true},
		{name: "active terminating Pod", phase: corev1.PodRunning, terminating: true, wantDrain: true},
		{name: "cannot inspect Pods", listError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cfg, c := upgradeFixture(t)
			d := resources.ListenerDeployment(cfg)
			if err := c.Create(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "listener", Namespace: cfg.Namespace, Labels: d.Spec.Selector.MatchLabels, Finalizers: []string{"test-drain"}},
				Status:     corev1.PodStatus{Phase: tc.phase},
			}
			if err := c.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if tc.terminating {
				if err := c.Delete(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			}
			if tc.listError {
				r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("cannot list Pods")
				}})
			}
			result, err := r.waitForDeployments(t.Context(), cfg, []deploymentWait{{d.Name, reasonWaitingForListener, msgWaitingForListener, "Listener"}})
			if (err != nil) != tc.listError {
				t.Fatalf("unexpected Pod inspection error: %v", err)
			}
			if tc.listError {
				return
			}
			wantReason := reasonWaitingForListener
			if tc.wantDrain {
				wantReason = reasonWaitingForOldPods
			}
			available := apimeta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionAvailable)
			if result.IsZero() || available == nil || available.Reason != wantReason {
				t.Fatalf("unexpected readiness wait: result=%+v available=%+v", result, available)
			}
		})
	}
}
