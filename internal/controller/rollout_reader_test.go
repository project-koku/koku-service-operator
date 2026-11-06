package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cost "github.com/project-koku/koku-service-operator/api/v1alpha1"
	"github.com/project-koku/koku-service-operator/internal/resources"
)

// Other resources use the ordinary fake fixture. ReplicaSet reads use real
// controller-runtime clients so an accidental cached Get starts an informer,
// exposing the get-only RBAC mismatch that a fake client cannot reproduce.
type rolloutReplicaSetClient struct {
	client.Client
	replicaSets client.Reader
}

func TestReconcileUsesPersistedMigrationFailure(t *testing.T) {
	r, cfg, storage := upgradeFixture(t)
	cfg.Status.Upgrade = cost.UpgradeStatus{}
	cfg.Finalizers = []string{finalizerName}
	if err := storage.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r.Client = &staleCMSCClient{Client: storage, stale: cfg.DeepCopy()}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "failed-migration", UID: "failed-attempt"}}
	r.phasesOverride = func(_ context.Context, current *cost.CostManagementServiceConfig) []PhaseFn {
		return []PhaseFn{func() (Result, error) { return r.migrationFailed(current, job), nil }}
	}
	for range 2 {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cfg)}); err != nil {
			t.Fatal(err)
		}
	}
	events := r.Recorder.(*record.FakeRecorder).Events
	count := 0
	for len(events) > 0 {
		if strings.Contains(<-events, "Warning MigrationFailed") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("stale cache repeated a persisted migration failure: %d Events", count)
	}
}

func (c rolloutReplicaSetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*appsv1.ReplicaSet); ok {
		return c.replicaSets.Get(ctx, key, obj, opts...)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type replicaSetGetOnlyAPI struct {
	object      *appsv1.ReplicaSet
	missing     bool
	namedGets   atomic.Int32
	deniedLists atomic.Int32
}

func (a *replicaSetGetOnlyAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	code := http.StatusForbidden
	var body any = &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
		Message: "ReplicaSet list/watch is forbidden by the operator permission contract"}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/replicasets/%s", a.object.Namespace, a.object.Name)
	if req.Method == http.MethodGet && req.URL.Path == path {
		a.namedGets.Add(1)
		code, body = http.StatusOK, a.object
		if a.missing {
			code = http.StatusNotFound
			body = &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
		}
	} else {
		a.deniedLists.Add(1)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(data))), Request: req}, nil
}

func TestRolloutReplicaSetGetOnly(t *testing.T) {
	for _, phase := range []string{"initial", "steady", "upgrade"} {
		for _, replica := range []string{"current", "old", "missing"} {
			t.Run(phase+"/"+replica, func(t *testing.T) {
				r, cfg, storage := upgradeFixture(t)
				switch phase {
				case "initial":
					cfg.Status.Upgrade.Installed = nil
				case "upgrade":
					cfg.Status.Upgrade.Operation = &cost.UpgradeOperation{Stage: cost.UpgradeStageRollingOut}
				}
				for _, d := range resources.ApplicationDeployments(cfg) {
					d.Annotations = map[string]string{"deployment.kubernetes.io/revision": "2"}
					if err := storage.Create(t.Context(), d); err != nil {
						t.Fatal(err)
					}
					markDeploymentReady(t, storage, cfg.Namespace, d.Name)
				}
				d := resources.KokuAPIDeployment(cfg)
				rs := &appsv1.ReplicaSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: replicaSetKind},
					ObjectMeta: metav1.ObjectMeta{Name: "operand-rs", Namespace: cfg.Namespace, UID: "replica-uid",
						ResourceVersion: "1", Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
				if replica == "old" {
					rs.Annotations["deployment.kubernetes.io/revision"] = "1"
				}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "operand-pod", Namespace: cfg.Namespace,
					Labels: d.Spec.Selector.MatchLabels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1",
						Kind: replicaSetKind, Name: rs.Name, UID: rs.UID, Controller: new(true)}}},
					Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if err := storage.Create(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
				transport := &replicaSetGetOnlyAPI{object: rs, missing: replica == "missing"}
				mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{appsv1.SchemeGroupVersion})
				mapper.Add(appsv1.SchemeGroupVersion.WithKind(replicaSetKind), meta.RESTScopeNamespace)
				api := &rest.Config{Host: "https://in-process-api.invalid", Transport: transport}
				watchCache, err := cache.New(api, cache.Options{Scheme: r.Scheme, Mapper: mapper})
				if err != nil {
					t.Fatal(err)
				}
				cacheCtx, stopCache := context.WithCancel(t.Context())
				defer stopCache()
				cacheDone := make(chan error, 1)
				go func() { cacheDone <- watchCache.Start(cacheCtx) }()
				t.Cleanup(func() {
					stopCache()
					if err := <-cacheDone; err != nil {
						t.Error(err)
					}
				})
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				if !watchCache.WaitForCacheSync(ctx) {
					t.Fatal("cache startup timed out")
				}
				cached, err := client.New(api, client.Options{Scheme: r.Scheme, Mapper: mapper,
					Cache: &client.CacheOptions{Reader: watchCache}})
				if err != nil {
					t.Fatal(err)
				}
				direct, err := client.New(api, client.Options{Scheme: r.Scheme, Mapper: mapper})
				if err != nil {
					t.Fatal(err)
				}
				r.Client = rolloutReplicaSetClient{Client: storage, replicaSets: cached}
				r.APIReader = rolloutReplicaSetClient{Client: storage, replicaSets: direct}
				result, err := r.waitForApplicationRollout(ctx, cfg)
				if err != nil {
					t.Fatalf("rollout requires more than named ReplicaSet get: %v", err)
				}
				if result.IsZero() != (replica == "current") {
					t.Fatalf("unexpected rollout completion for %s ReplicaSet: %+v", replica, result)
				}
				if transport.namedGets.Load() != 1 || transport.deniedLists.Load() != 0 {
					t.Fatalf("ReplicaSet API calls: named gets=%d, forbidden informer requests=%d",
						transport.namedGets.Load(), transport.deniedLists.Load())
				}
			})
		}
	}
}

func TestRolloutWaitsForTerminatingPodsBeforeReadiness(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		t.Run(fmt.Sprint(terminating), func(t *testing.T) {
			r, cfg, storage := upgradeFixture(t)
			d := resources.ApplicationDeployments(cfg)[0]
			if err := storage.Create(t.Context(), d); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "draining", Namespace: cfg.Namespace,
				Labels: d.Spec.Selector.MatchLabels}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			if terminating {
				pod.Finalizers = []string{"test-drain"}
			}
			if err := storage.Create(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if terminating {
				if err := storage.Delete(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.waitForApplicationRollout(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(cfg.Status.Conditions, cost.ConditionAvailable)
			want := reasonWaitingForApplicationRollout
			if terminating {
				want = reasonWaitingForOldPods
			}
			if condition == nil || condition.Reason != want {
				t.Fatalf("readiness gate hid graceful shutdown: %+v", condition)
			}
			if !holdingWorkerReadinessWait(cfg) {
				t.Fatal("core reconciliation would reset the rollout wait clock")
			}
		})
	}
}
