package manager

import (
	"context"
	"log/slog"
	"path"
	"testing"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	shimv1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestPodReconcilerMigrationSource(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.Migration{}).Build()
	ctx := context.Background()

	for name, tc := range map[string]struct {
		pod                 *corev1.Pod
		containerID         string
		expectedContainerID string
		deletePod           bool
		nodeName            string
		runtimeClassName    string
		expectedMigration   bool
		expectedRequeue     bool
		garbageCollection   bool
	}{
		"pod that should be migrated": {
			pod:                 newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_SCALED_DOWN),
			containerID:         "containerd://imageid",
			expectedContainerID: "imageid",
			deletePod:           true,
			nodeName:            "node1",
			expectedMigration:   true,
			garbageCollection:   true,
		},
		"pod without live migration and running": {
			pod:                 newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING),
			containerID:         "containerd://imageid",
			expectedContainerID: "imageid",
			deletePod:           true,
			nodeName:            "node1",
			expectedMigration:   true,
		},
		"pod with live migration and running": {
			pod:                 enableLiveMigration(newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING)),
			containerID:         "containerd://imageid",
			expectedContainerID: "imageid",
			deletePod:           true,
			nodeName:            "node1",
			expectedMigration:   true,
		},
		"pod on wrong node": {
			pod:               newMigratePod("node2", v1.RuntimeClassName, shimv1.ContainerPhase_SCALED_DOWN),
			deletePod:         true,
			nodeName:          "node1",
			expectedMigration: false,
		},
		"not a zeropod": {
			pod:               newMigratePod("node1", "", shimv1.ContainerPhase_SCALED_DOWN),
			deletePod:         true,
			nodeName:          "node1",
			expectedMigration: false,
		},
		"not deleted": {
			pod:               newMigratePod("node1", "", shimv1.ContainerPhase_SCALED_DOWN),
			deletePod:         false,
			nodeName:          "node1",
			expectedMigration: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(nodev1.NodeNameEnvKey, tc.nodeName)
			r, err := newPodReconciler(kube, slog.Default())
			require.NoError(t, err)
			r.autoGCMigrations = tc.garbageCollection

			if tc.containerID != "" {
				tc.pod.Status.ContainerStatuses[0].ContainerID = tc.containerID
			}
			require.NoError(t, kube.Create(ctx, tc.pod))
			if tc.deletePod {
				// set a finalizer to simulate deletion
				tc.pod.SetFinalizers([]string{"foo"})
				require.NoError(t, kube.Update(ctx, tc.pod))
				require.NoError(t, kube.Delete(ctx, tc.pod))
			}
			res, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(tc.pod),
			})
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedRequeue, res.Requeue)

			if tc.expectedMigration {
				migration := &v1.Migration{}
				assert.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(tc.pod), migration))
				require.NotEmpty(t, migration.Spec.Containers)
				require.NotEmpty(t, migration.Status.Containers)
				assert.Equal(t, v1.MigrationPhasePending, migration.Status.Containers[0].Condition.Phase)
				assert.Equal(t, tc.expectedContainerID, migration.Spec.Containers[0].ID)
				hasRef, err := controllerutil.HasOwnerReference(migration.OwnerReferences, tc.pod, kube.Scheme())
				assert.NoError(t, err)
				assert.Equal(t, tc.garbageCollection, hasRef, "owner reference")
			} else {
				assert.True(t, errors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(tc.pod), &v1.Migration{})))
			}
		})
	}
}

func TestPodReconcilerMigrationTarget(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	ctx := context.Background()

	for name, tc := range map[string]struct {
		pod               *corev1.Pod
		existingMigration *v1.Migration
		expectClaimed     bool
		nodeName          string
		expectedRequeue   bool
		deletePod         bool
		garbageCollection bool
	}{
		"pending pod should claim migration with matching hash": {
			pod:               setPhase(setPodTemplateHash(newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING), "hash"), corev1.PodPending),
			existingMigration: newTestMigration("hash"),
			expectClaimed:     true,
			expectedRequeue:   false,
			nodeName:          "node1",
			garbageCollection: true,
		},
		"another pending pod should claim migration with matching hash": {
			pod:               setPhase(setPodTemplateHash(newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING), "hashb"), corev1.PodPending),
			existingMigration: newTestMigration("hashb"),
			expectClaimed:     true,
			expectedRequeue:   false,
			nodeName:          "node1",
		},
		"pending pod should not claim migration with non-matching hash": {
			pod:               setPhase(setPodTemplateHash(newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING), "different"), corev1.PodPending),
			existingMigration: newTestMigration("hash2"),
			expectClaimed:     false,
			expectedRequeue:   false,
			nodeName:          "node1",
		},
		"pod with empty node should not claim migration with matching hash": {
			pod:               setPhase(setPodTemplateHash(newMigratePod("", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING), "hash3"), corev1.PodPending),
			existingMigration: newTestMigration("hash3"),
			expectClaimed:     false,
			expectedRequeue:   false,
			nodeName:          "node1",
		},
		"running pod should not claim migration with matching hash": {
			pod:               setPhase(setPodTemplateHash(newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING), "hash4"), corev1.PodRunning),
			existingMigration: newTestMigration("hash4"),
			expectClaimed:     false,
			expectedRequeue:   false,
			nodeName:          "node1",
		},
		"deleting pod should not claim migration with matching hash": {
			pod:               setPhase(setPodTemplateHash(newMigratePod("node1", v1.RuntimeClassName, shimv1.ContainerPhase_RUNNING), "hash5"), corev1.PodPending),
			existingMigration: newTestMigration("hash5"),
			deletePod:         true,
			expectClaimed:     false,
			expectedRequeue:   false,
			nodeName:          "node1",
		},
	} {
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.Migration{}).Build()
		t.Run(name, func(t *testing.T) {
			require.NoError(t, kube.Create(ctx, tc.existingMigration))
			t.Setenv(nodev1.NodeNameEnvKey, tc.nodeName)
			r, err := newPodReconciler(kube, slog.Default())
			require.NoError(t, err)
			r.autoGCMigrations = tc.garbageCollection

			require.NoError(t, kube.Create(ctx, tc.pod))
			if tc.deletePod {
				// set a finalizer to simulate deletion
				tc.pod.SetFinalizers([]string{"foo"})
				require.NoError(t, kube.Update(ctx, tc.pod))
				require.NoError(t, kube.Delete(ctx, tc.pod))
			}

			res, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(tc.pod),
			})
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedRequeue, res.Requeue)

			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(tc.existingMigration), tc.existingMigration))
			if tc.expectClaimed {
				assert.Equal(t, tc.pod.Name, tc.existingMigration.Spec.TargetPod)
				assert.Equal(t, tc.pod.Spec.NodeName, tc.existingMigration.Spec.TargetNode)
			} else {
				assert.Empty(t, tc.existingMigration.Spec.TargetPod)
				assert.Empty(t, tc.existingMigration.Spec.TargetNode)
			}
			hasRef, err := controllerutil.HasOwnerReference(tc.existingMigration.OwnerReferences, tc.pod, kube.Scheme())
			assert.NoError(t, err)
			assert.Equal(t, tc.garbageCollection, hasRef, "owner reference")
		})
	}
}

func newMigratePod(nodeName, runtimeClassName string, phase shimv1.ContainerPhase) *corev1.Pod {
	pod := newPod(corev1.ResourceList{})
	pod.Name = ""
	pod.GenerateName = "controller-test-"
	pod.Spec.NodeName = nodeName
	pod.Spec.RuntimeClassName = ptr.To(runtimeClassName)
	containerName := pod.Spec.Containers[0].Name
	pod.SetAnnotations(map[string]string{
		nodev1.MigrateAnnotationKey: pod.Spec.Containers[0].Name,
	})
	pod.SetLabels(map[string]string{
		path.Join(StatusLabelKeyPrefix, containerName): phase.String(),
	})
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerName,
	}}
	return pod
}

func newTestMigration(podTemplateHash string) *v1.Migration {
	return &v1.Migration{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sourcepod-",
			Namespace:    "default",
		},
		Spec: v1.MigrationSpec{
			SourcePod:       "sourcepod",
			SourceNode:      "sourcenode",
			PodTemplateHash: podTemplateHash,
			Containers:      []v1.MigrationContainer{},
		},
	}
}

func enableLiveMigration(pod *corev1.Pod) *corev1.Pod {
	pod.Annotations[nodev1.LiveMigrateAnnotationKey] = pod.Spec.Containers[0].Name
	return pod
}

func setPodTemplateHash(pod *corev1.Pod, podTemplateHash string) *corev1.Pod {
	pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = podTemplateHash
	return pod
}

func setPhase(pod *corev1.Pod, phase corev1.PodPhase) *corev1.Pod {
	pod.Status.Phase = phase
	return pod
}
