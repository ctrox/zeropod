package node

import (
	"log/slog"
	"testing"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/manager/capacity"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSetContainerStatus(t *testing.T) {
	migration := &v1.Migration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Status: v1.MigrationStatus{
			Containers: []v1.MigrationContainerStatus{
				{
					Name:     "container1",
					PausedAt: metav1.NowMicro(),
				},
			},
		},
	}

	setOrUpdateContainerStatus(migration, "container1", func(cms *v1.MigrationContainerStatus) {
		cms.Condition.Phase = v1.MigrationPhaseCompleted
		cms.RestoredAt = metav1.NowMicro()
	})
	assert.Equal(t, v1.MigrationPhaseCompleted, migration.Status.Containers[0].Condition.Phase)
	assert.NotEmpty(t, migration.Status.Containers[0].PausedAt)
	assert.NotEmpty(t, migration.Status.Containers[0].RestoredAt)

	setOrUpdateContainerStatus(migration, "container2", func(cms *v1.MigrationContainerStatus) {
		cms.Condition.Phase = v1.MigrationPhaseFailed
		cms.RestoredAt = metav1.NowMicro()
	})
	assert.Len(t, migration.Status.Containers, 2)
	assert.Equal(t, v1.MigrationPhaseFailed, migration.Status.Containers[1].Condition.Phase)
	assert.NotEmpty(t, migration.Status.Containers[1].RestoredAt)
}

func TestRestoreCapacity(t *testing.T) {
	for name, tc := range map[string]struct {
		node          *corev1.Node
		pod           *corev1.Pod
		migration     *v1.Migration
		containerName string
		allowed       bool
		expectedErr   bool
		podScaledDown bool
	}{
		"no container": {
			node: newNode("empty", nil),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
			},
			allowed:     true,
			expectedErr: true,
		},
		"empty node": {
			node: newNode("empty", nil),
			pod: newPod("app1", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			}),
			containerName: "app1",
			allowed:       true,
		},
		"node with enough capacity": {
			node: newNode("empty", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("20Gi"),
			}),
			pod: newPod("app1", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("20Gi"),
			}),
			containerName: "app1",
			allowed:       true,
		},
		"node at memory capacity": {
			node: newNode("full-mem", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			}),
			pod: newPod("app1", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("20Gi"),
			}),
			migration:     newMigration("10.0.0.1"),
			containerName: "app1",
			allowed:       false,
		},
		"pod with resources in annotation": {
			node: newNode("full-mem", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			}),
			pod: newPod("app1", corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("20Gi"),
			}),
			migration:     newMigration("10.0.0.1"),
			containerName: "app1",
			allowed:       false,
			podScaledDown: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			assert.NoError(t, corev1.AddToScheme(scheme))
			assert.NoError(t, v1.AddToScheme(scheme))
			if tc.podScaledDown {
				assert.NoError(t, setScaledDown(tc.pod))
			}
			objs := []client.Object{tc.node, tc.pod}
			if tc.migration != nil {
				objs = append(objs, tc.migration)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			cap := capacity.NewNodeTracker(prometheus.NewRegistry(), "name")
			for name, q := range tc.node.Status.Capacity {
				cap.SetCapacity(name, q)
			}
			slog.SetLogLoggerLevel(slog.LevelDebug)
			ns := &nodeService{
				kube:     kube,
				nodeName: tc.node.Name,
				log:      slog.Default(),
				cap:      cap,
			}
			resp, err := ns.RestoreCapacity(t.Context(), &nodev1.RestoreCapacityRequest{
				PodInfo: &nodev1.PodInfo{
					Name:          tc.pod.Name,
					Namespace:     tc.pod.Namespace,
					ContainerName: tc.containerName,
				},
			})
			if tc.expectedErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.NotEmpty(t, resp)
			assert.Equal(t, tc.allowed, resp.Allowed)
			if tc.allowed {
				assert.Empty(t, resp.RedirectAddr)
			}
		})
	}
}

func newPod(containerName string, requests corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: containerName,
				Resources: corev1.ResourceRequirements{
					Requests: requests,
				},
			}},
		},
	}
}

func setScaledDown(pod *corev1.Pod) error {
	if err := manager.SetAnnotations(pod); err != nil {
		return err
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Resources.Requests[corev1.ResourceCPU] = manager.ScaledDownCPU
		pod.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory] = manager.ScaledDownMemory
	}
	return nil
}

func newNode(name string, cap corev1.ResourceList) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: cap,
		},
	}
}

func newMigration(targetPodIP string) *v1.Migration {
	return &v1.Migration{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"},
		Spec: v1.MigrationSpec{
			TargetPodIP: targetPodIP,
		},
	}
}
