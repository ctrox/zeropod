package manager

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const defaultSourceNode = "foo"

func TestMigrationReconcilerCheckpointCleanup(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.Migration{}).Build()
	ctx := context.Background()
	nodev1.SetImageBasePath(t.TempDir())

	for name, tc := range map[string]struct {
		sourceNodeName            string
		migration                 *v1.Migration
		migrationStatus           *v1.MigrationStatus
		expectedRequeue           bool
		expectedFinalizer         bool
		expectedCheckpointCleaned bool
		delete                    bool
	}{
		"migration matches node": {
			migration:         newTestMigrationWithSourceNode(defaultSourceNode),
			sourceNodeName:    defaultSourceNode,
			expectedFinalizer: true,
		},
		"migration does not match node": {
			migration:         newTestMigrationWithSourceNode("bar"),
			sourceNodeName:    defaultSourceNode,
			expectedFinalizer: false,
		},
		"checkpoint of completed migration is cleaned": {
			migration:                 newTestMigrationWithContainer("c1"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: true,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhaseCompleted},
				},
			}},
		},
		"checkpoint of failed migration is cleaned": {
			migration:                 newTestMigrationWithContainer("c1"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: true,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhaseFailed},
				},
			}},
		},
		"checkpoints of multiple failed migration containers is cleaned": {
			migration:                 newTestMigrationWithContainer("c1", "c2"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: true,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhaseFailed},
				},
				{
					Name:      "c2",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhaseFailed},
				},
			}},
		},
		"checkpoints of mixed state migration containers is cleaned": {
			migration:                 newTestMigrationWithContainer("c1", "c2"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: true,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhaseFailed},
				},
				{
					Name:      "c2",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhaseCompleted},
				},
			}},
		},
		"checkpoint of pending migration is not cleaned": {
			migration:                 newTestMigrationWithContainer("c1"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: false,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhasePending},
				},
			}},
		},
		"checkpoint of running migration is not cleaned": {
			migration:                 newTestMigrationWithContainer("c1"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: false,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhasePending},
				},
			}},
		},
		"checkpoint of pending deleted migration is cleaned": {
			migration:                 newTestMigrationWithContainer("c1"),
			expectedFinalizer:         true,
			expectedCheckpointCleaned: true,
			delete:                    true,
			migrationStatus: &v1.MigrationStatus{Containers: []v1.MigrationContainerStatus{
				{
					Name:      "c1",
					Condition: v1.MigrationCondition{Phase: v1.MigrationPhasePending},
				},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, container := range tc.migration.Spec.Containers {
				os.MkdirAll(nodev1.ImagePath(container.ID), os.ModeDir)
			}
			if tc.sourceNodeName == "" {
				tc.sourceNodeName = defaultSourceNode
			}
			r := newMigrationReconciler(kube, slog.Default(), tc.sourceNodeName)

			require.NoError(t, kube.Create(ctx, tc.migration))
			if tc.migrationStatus != nil {
				tc.migration.Status = *tc.migrationStatus
				require.NoError(t, kube.Status().Update(ctx, tc.migration))
			}
			res, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(tc.migration),
			})
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedRequeue, res.Requeue)

			require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(tc.migration), tc.migration))
			assert.Equal(t, tc.expectedFinalizer, controllerutil.ContainsFinalizer(tc.migration, checkpointCleanupFinalizer))

			if tc.delete {
				require.NoError(t, kube.Delete(ctx, tc.migration))
				res, err = r.Reconcile(ctx, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(tc.migration),
				})
				assert.NoError(t, err)
			}

			for _, container := range tc.migration.Spec.Containers {
				_, err := os.Stat(nodev1.ImagePath(container.ID))
				if tc.expectedCheckpointCleaned {
					assert.Error(t, err, "checkpoint path is not cleaned")
				} else {
					assert.NoError(t, err, "checkpoint path was cleaned")
				}
			}
		})
	}
}

func newTestMigrationWithSourceNode(sourceNode string) *v1.Migration {
	migration := newTestMigration("")
	migration.Spec.SourceNode = sourceNode
	return migration
}

func newTestMigrationWithContainer(names ...string) *v1.Migration {
	migration := newTestMigration("")
	migration.Spec.SourceNode = defaultSourceNode
	for _, name := range names {
		migration.Spec.Containers = append(migration.Spec.Containers, v1.MigrationContainer{
			// random id to avoid test clashes
			ID:   strconv.FormatInt(time.Now().UnixNano(), 10),
			Name: name,
		})
	}
	return migration
}
