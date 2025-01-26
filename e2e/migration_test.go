package e2e

import (
	"context"
	"testing"
	"time"

	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test")
	}
	e2e := setupOnce(t)
	ctx := context.Background()

	type testCase struct {
		deploy          *appsv1.Deployment
		svc             *corev1.Service
		sameNode        bool
		migrationCount  int
		beforeMigration func(t *testing.T)
		afterMigration  func(t *testing.T)
	}
	cases := map[string]testCase{
		"same-node live migration": {
			deploy:         freezerDeployment("same-node-migration", "default", 256),
			svc:            testService(8080),
			sameNode:       true,
			migrationCount: 3,
			beforeMigration: func(t *testing.T) {
				require.NoError(t, freezerWrite(t.Name(), e2e.port))
			},
			afterMigration: func(t *testing.T) {
				f, err := freezerRead(e2e.port)
				require.NoError(t, err)
				t.Logf("freeze duration: %s", f.LastFreezeDuration)
				assert.Equal(t, t.Name(), f.Data, "freezer memory has persisted migration")
				assert.Less(t, f.LastFreezeDuration, time.Second, "freeze duration")
			},
		},
		"cross-node live migration": {
			deploy:         freezerDeployment("node-migration", "default", 256),
			svc:            testService(8080),
			sameNode:       false,
			migrationCount: 3,
			beforeMigration: func(t *testing.T) {
				require.NoError(t, freezerWrite(t.Name(), e2e.port))
			},
			afterMigration: func(t *testing.T) {
				f, err := freezerRead(e2e.port)
				require.NoError(t, err)
				t.Logf("freeze duration: %s", f.LastFreezeDuration)
				assert.Equal(t, t.Name(), f.Data, "freezer memory has persisted migration")
				assert.Less(t, f.LastFreezeDuration, time.Second, "freeze duration")
			},
		},
		"cross-node 1 GiB live migration": {
			deploy:         freezerDeployment("node-migration-1gib", "default", 1024),
			svc:            testService(8080),
			sameNode:       false,
			migrationCount: 1,
			beforeMigration: func(t *testing.T) {
				require.NoError(t, freezerWrite(t.Name(), e2e.port))
			},
			afterMigration: func(t *testing.T) {
				f, err := freezerRead(e2e.port)
				require.NoError(t, err)
				t.Logf("freeze duration: %s", f.LastFreezeDuration)
				assert.Equal(t, t.Name(), f.Data, "freezer memory has persisted migration")
				assert.Less(t, f.LastFreezeDuration, time.Second, "freeze duration")
			},
		},
	}

	migrate := func(t *testing.T, ctx context.Context, e2e *e2eConfig, tc testCase) {
		pods := podsOfDeployment(t, ctx, e2e.client, tc.deploy)
		if len(pods) < 1 {
			t.Fatal("expected at least one pod in the deployment")
		}
		pod := pods[0]

		if tc.sameNode {
			uncordon := cordonOtherNodes(t, ctx, e2e.client, pod.Spec.NodeName)
			defer uncordon()
		} else {
			uncordon := cordonNode(t, ctx, e2e.client, pod.Spec.NodeName)
			defer uncordon()
		}

		assert.NoError(t, e2e.client.Delete(ctx, &pod))
		assert.Eventually(t, func() bool {
			pods := podsOfDeployment(t, ctx, e2e.client, tc.deploy)
			if len(pods) != 1 {
				return false
			}
			migration := &v1.Migration{}
			if err := e2e.client.Get(ctx, objectName(&pod), migration); err != nil {
				return false
			}
			if len(migration.Status.Containers) == 0 {
				return false
			}
			require.NotEqual(t, v1.MigrationPhaseFailed, migration.Status.Containers[0].Condition.Phase)
			t.Logf("migration phase: %s", migration.Status.Containers[0].Condition.Phase)
			return pods[0].Status.Phase == corev1.PodRunning &&
				migration.Status.Containers[0].Condition.Phase == v1.MigrationPhaseCompleted
		}, time.Minute, time.Second)

		waitForService(t, ctx, e2e.client, tc.svc, 1)
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.svc == nil {
				tc.svc = testService(defaultTargetPort)
			}

			cleanupPod := createDeployAndWait(t, ctx, e2e.client, tc.deploy)
			cleanupService := createServiceAndWait(t, ctx, e2e.client, tc.svc, 1)
			defer cleanupPod()
			defer cleanupService()

			for range tc.migrationCount {
				tc.beforeMigration(t)
				checkCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				go func() {
					downtime := availabilityCheck(checkCtx, e2e.port)
					t.Logf("downtime was: %s", downtime)
				}()
				migrate(t, ctx, e2e, tc)
				cancel()
				tc.afterMigration(t)
			}
		})
	}
}
