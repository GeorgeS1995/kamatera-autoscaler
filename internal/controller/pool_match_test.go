package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
)

func mustValidate(t *testing.T, cfg *config.Config) *config.Config {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return cfg
}

func cfgWithPools() *config.Config {
	return &config.Config{
		Datacenter:            "EU-FR",
		CloudInitTemplatePath: "/x",
		Pools: []config.Pool{
			{Name: "general", CPUType: "B", CPUCores: 2, RAMMB: 2048, DiskGB: 20, Image: "img", MinNodes: 1, MaxNodes: 4, NodeLabels: "pool=general", PodSelector: "pool=general"},
			{Name: "gpu-pool", CPUType: "D", CPUCores: 4, RAMMB: 8192, DiskGB: 60, Image: "img", MinNodes: 0, MaxNodes: 3, NodeLabels: "pool=gpu-pool,workload=gpu", PodSelector: "workload=gpu"},
		},
	}
}

func TestPoolForPod(t *testing.T) {
	cfg := mustValidate(t, cfgWithPools())
	cases := []struct {
		name        string
		nodeSel     map[string]string
		wantPool    string
		wantNoMatch bool
	}{
		{name: "match general", nodeSel: map[string]string{"pool": "general"}, wantPool: "general"},
		{name: "match gpu via workload", nodeSel: map[string]string{"workload": "gpu"}, wantPool: "gpu-pool"},
		{name: "no nodeSelector", nodeSel: nil, wantNoMatch: true},
		{name: "unknown selector", nodeSel: map[string]string{"pool": "phantom"}, wantNoMatch: true},
		{name: "extra labels still match", nodeSel: map[string]string{"workload": "gpu", "extra": "x"}, wantPool: "gpu-pool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := corev1.Pod{Spec: corev1.PodSpec{NodeSelector: tc.nodeSel}}
			got := poolForPod(pod, cfg)
			if tc.wantNoMatch {
				if got != nil {
					t.Errorf("expected no match, got %s", got.Name)
				}
				return
			}
			if got == nil || got.Name != tc.wantPool {
				t.Errorf("got %v, want %s", got, tc.wantPool)
			}
		})
	}
}

func TestPoolForNode(t *testing.T) {
	cfg := mustValidate(t, cfgWithPools())
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "gpu-pool"}}}
	if got := poolForNode(n, cfg); got == nil || got.Name != "gpu-pool" {
		t.Errorf("got %v, want gpu-pool", got)
	}
	n.Labels = map[string]string{}
	if poolForNode(n, cfg) != nil {
		t.Error("expected nil for node without pool label")
	}
	n.Labels = map[string]string{"pool": "phantom"}
	if poolForNode(n, cfg) != nil {
		t.Error("expected nil for unknown pool")
	}
}

func TestIsPendingForResources(t *testing.T) {
	cases := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{
			name: "running pod",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: false,
		},
		{
			name: "pending insufficient cpu",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes are available: 3 Insufficient cpu",
				}},
			}},
			want: true,
		},
		{
			name: "pending insufficient memory",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable, Message: "Insufficient memory",
				}},
			}},
			want: true,
		},
		{
			name: "pending volume mount",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable, Message: "VolumeBinding failed",
				}},
			}},
			want: false,
		},
		{
			name: "pending no condition",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPendingForResources(tc.pod); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
