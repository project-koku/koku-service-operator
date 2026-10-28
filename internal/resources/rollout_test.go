package resources

import (
	"strings"
	"testing"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/utils/ptr"
)

func TestApplicationRolloutPolicies(t *testing.T) {
	cfg := minimalCRForResources("cm", "ns")
	cfg.Spec.ROS.Enabled = new(true)
	cfg.Spec.RBAC.Image = cost.ImageSpec{Repository: "rbac", Tag: "v1"}
	cfg.Spec.ROS.Image = cost.ImageSpec{Repository: "ros", Tag: "v1"}
	workers := map[string]bool{
		"cost-scheduler": true, "listener": true, "rbac-worker": true,
		"ros-processor": true, "ros-recommendation-poller": true, "ros-housekeeper": true,
		"cost-worker-celery": true, "cost-worker-priority": true, "cost-worker-summary": true,
		"cost-worker-ocp": true, "cost-worker-cost-model": true, "cost-worker-refresh": true,
		"cost-worker-hcs": true, "cost-worker-download": true,
		"cost-worker-subs-extraction": true, "cost-worker-subs-transmission": true,
	}
	for _, d := range ApplicationDeployments(cfg) {
		component := d.Labels[labelComponent]
		worker := workers[component]
		surge, unavailable, grace := "25%", "25%", int64(60)
		if worker {
			surge, unavailable, grace = "0", "1", 3600
		}
		policy := d.Spec.Strategy
		if policy.Type != appsv1.RollingUpdateDeploymentStrategyType || policy.RollingUpdate == nil {
			t.Fatalf("%s has no explicit rolling strategy", d.Name)
		}
		if policy.RollingUpdate.MaxSurge.String() != surge || policy.RollingUpdate.MaxUnavailable.String() != unavailable {
			t.Errorf("wrong policy for %s: %+v", d.Name, policy)
		}
		if d.Spec.Template.Spec.TerminationGracePeriodSeconds == nil || *d.Spec.Template.Spec.TerminationGracePeriodSeconds != grace {
			t.Errorf("wrong shutdown budget for %s", d.Name)
		}
		if d.Spec.ProgressDeadlineSeconds == nil || int64(*d.Spec.ProgressDeadlineSeconds) <= grace {
			t.Errorf("%s progress deadline is shorter than graceful shutdown", d.Name)
		}
		if d.Spec.Template.Spec.Containers[0].ReadinessProbe == nil {
			t.Errorf("%s has no readiness check", d.Name)
		}
	}
	worker := CeleryWorkerDeployment(cfg, "celery", cost.CeleryWorkerSpec{Replicas: ptr.To[int32](1)})
	if !strings.Contains(worker.Spec.Template.Spec.Containers[0].Command[2], "exec celery") {
		t.Fatal("worker shell does not hand SIGTERM to Celery")
	}
	beat := CeleryBeatDeployment(cfg).Spec.Template.Spec.Containers[0]
	if !strings.Contains(beat.Command[2], "exec celery") || !strings.Contains(beat.Command[2], "--pidfile=") {
		t.Fatal("Beat must exec Celery and publish its startup PID")
	}
	rbacProbe := RBACWorkerDeployment(cfg).Spec.Template.Spec.Containers[0].ReadinessProbe
	if rbacProbe.Exec == nil || len(rbacProbe.Exec.Command) != 3 || rbacProbe.Exec.Command[0] != "python" ||
		rbacProbe.TimeoutSeconds < 10 {
		t.Fatal("RBAC readiness must work without shell utilities and allow Django startup plus the ping timeout")
	}
	listener := ListenerDeployment(cfg).Spec.Template.Spec.Containers[0]
	if !strings.Contains(listener.Command[2], "signal.signal(signal.SIGTERM, signal.default_int_handler)") ||
		!strings.Contains(listener.Command[2], "exec python") {
		t.Fatal("Python PID 1 must handle SIGTERM through the listener's normal interrupt shutdown path")
	}
	if listener.ReadinessProbe == nil || listener.ReadinessProbe.HTTPGet == nil ||
		listener.ReadinessProbe.HTTPGet.Path != "/readyz" || listener.ReadinessProbe.HTTPGet.Port.IntValue() != 9000 {
		t.Fatal("listener readiness must use Koku's probe server on port 9000")
	}
	if beat.ReadinessProbe == nil || beat.ReadinessProbe.Exec == nil {
		t.Fatal("Beat must have an exec readiness probe")
	}
	beatProbe := strings.Join(beat.ReadinessProbe.Exec.Command, " ")
	if !strings.Contains(beatProbe, "kill -0 \"$pid\"") || strings.Contains(beatProbe, "= 1") {
		t.Fatal("Beat readiness must verify the pidfile process without assuming it is PID 1")
	}
}
