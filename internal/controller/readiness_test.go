package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsDeploymentReady(t *testing.T) {
	tests := []struct {
		name    string
		deploy  *appsv1.Deployment
		want    bool
		wantErr bool
	}{
		{
			name: "ready",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns"},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(2))},
				Status:     appsv1.DeploymentStatus{AvailableReplicas: 2},
			},
			want: true,
		},
		{
			name: "not ready",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns"},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(2))},
				Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
			},
			want: false,
		},
		{
			name: "zero replicas is ready",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns"},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(0))},
			},
			want: true,
		},
		{
			name: "nil replicas is ready",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns"},
			},
			want: true,
		},
		{
			name: "not found",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder()
			if tt.deploy != nil {
				b = b.WithObjects(tt.deploy)
			}
			r := &CostManagementServiceConfigReconciler{Client: b.Build()}
			got, err := r.isDeploymentReady(context.Background(), "ns", "api")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadinessBackoff(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want time.Duration
	}{
		{name: "just past timeout", age: 5*time.Minute + 10*time.Second, want: 30 * time.Second},
		{name: "under one minute past timeout", age: 5*time.Minute + 45*time.Second, want: time.Minute},
		{name: "under three minutes past timeout", age: 7 * time.Minute, want: 2 * time.Minute},
		{name: "well past timeout", age: 11 * time.Minute, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readinessBackoff(time.Now().Add(-tt.age))
			if got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestIsStatefulSetReady(t *testing.T) {
	tests := []struct {
		name    string
		ss      *appsv1.StatefulSet
		want    bool
		wantErr bool
	}{
		{
			name: "ready",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Generation: 1},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(1))},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1,
					ReadyReplicas:      1,
					UpdatedReplicas:    1,
					CurrentRevision:    "rev-a",
					UpdateRevision:     "rev-a",
				},
			},
			want: true,
		},
		{
			name: "not ready",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(1))},
				Status:     appsv1.StatefulSetStatus{ReadyReplicas: 0},
			},
			want: false,
		},
		{
			name: "stale observed generation",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Generation: 2},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(1))},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 1,
					ReadyReplicas:      1,
					UpdatedReplicas:    1,
					CurrentRevision:    "rev-a",
					UpdateRevision:     "rev-a",
				},
			},
			want: false,
		},
		{
			name: "stale pods during rollout",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Generation: 2},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(1))},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					ReadyReplicas:      1,
					UpdatedReplicas:    0,
					CurrentRevision:    "rev-old",
					UpdateRevision:     "rev-new",
				},
			},
			want: false,
		},
		{
			name: "updated replicas lag desired",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Generation: 2},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(2))},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					ReadyReplicas:      2,
					UpdatedReplicas:    1,
					CurrentRevision:    "rev-a",
					UpdateRevision:     "rev-a",
				},
			},
			want: false,
		},
		{
			name: "zero replicas is ready",
			ss: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"},
				Spec:       appsv1.StatefulSetSpec{Replicas: new(int32(0))},
			},
			want: true,
		},
		{
			name: "not found",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder()
			if tt.ss != nil {
				b = b.WithObjects(tt.ss)
			}
			r := &CostManagementServiceConfigReconciler{Client: b.Build()}
			got, err := r.isStatefulSetReady(context.Background(), "ns", "db")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJobConditionHelpers(t *testing.T) {
	tests := []struct {
		name         string
		conditions   []batchv1.JobCondition
		wantComplete bool
		wantFailed   bool
	}{
		{
			name: "complete",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
			wantComplete: true,
		},
		{
			name: "failed",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			wantFailed: true,
		},
		{
			name: "complete false",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
			},
		},
		{
			name: "failed false",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
			},
		},
		{
			name: "no conditions",
		},
		{
			name: "both true",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			wantComplete: true,
			wantFailed:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: tt.conditions}}
			if got := isJobComplete(job); got != tt.wantComplete {
				t.Errorf("isJobComplete = %v, want %v", got, tt.wantComplete)
			}
			if got := isJobFailed(job); got != tt.wantFailed {
				t.Errorf("isJobFailed = %v, want %v", got, tt.wantFailed)
			}
		})
	}
}
