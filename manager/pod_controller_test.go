package manager

import (
	"context"
	"log/slog"
	"testing"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestPodReconciler(t *testing.T) {
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
	}{
		"pod that should be migrated": {
			pod:                 newMigratePod("node1", v1.RuntimeClassName),
			containerID:         "containerd://imageid",
			expectedContainerID: "imageid",
			deletePod:           true,
			nodeName:            "node1",
			expectedMigration:   true,
		},
		"pod on wrong node": {
			pod:               newMigratePod("node2", v1.RuntimeClassName),
			deletePod:         true,
			nodeName:          "node1",
			expectedMigration: false,
		},
		"not a zeropod": {
			pod:               newMigratePod("node1", ""),
			deletePod:         true,
			nodeName:          "node1",
			expectedMigration: false,
		},
		"not deleted": {
			pod:               newMigratePod("node1", ""),
			deletePod:         false,
			nodeName:          "node1",
			expectedMigration: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(nodev1.NodeNameEnvKey, tc.nodeName)
			r, err := newPodReconciler(kube, slog.Default())
			require.NoError(t, err)

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
			} else {
				assert.True(t, errors.IsNotFound(kube.Get(ctx, client.ObjectKeyFromObject(tc.pod), &v1.Migration{})))
			}
		})
	}
}

func newMigratePod(nodeName, runtimeClassName string) *corev1.Pod {
	pod := newPod(corev1.ResourceList{})
	pod.Name = ""
	pod.GenerateName = "controller-test-"
	pod.Spec.NodeName = nodeName
	pod.Spec.RuntimeClassName = ptr.To(runtimeClassName)
	pod.SetAnnotations(map[string]string{nodev1.MigrateAnnotationKey: pod.Spec.Containers[0].Name})
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: pod.Spec.Containers[0].Name,
	}}
	return pod
}
