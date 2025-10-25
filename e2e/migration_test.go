package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	shimv1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/manager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

type testCase struct {
	deploy                *appsv1.Deployment
	svc                   *corev1.Service
	sameNode              bool
	migrationCount        int
	liveMigration         bool
	expectDataNotMigrated bool
	beforeMigration       func(t *testing.T)
	afterMigration        func(t *testing.T, tc testCase)
}

func TestMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test")
	}
	e2e := setupOnce(t)
	ctx := context.Background()
	cases := map[string]testCase{
		"same-node live migration": {
			deploy:         freezerDeployment("same-node-live-migration", "default", 256, liveMigrateAnnotation("freezer")),
			svc:            testService(8080),
			sameNode:       true,
			liveMigration:  true,
			migrationCount: 3,
		},
		"cross-node live migration": {
			deploy:         freezerDeployment("node-migration", "default", 256, liveMigrateAnnotation("freezer")),
			svc:            testService(8080),
			sameNode:       false,
			liveMigration:  true,
			migrationCount: 3,
		},
		"cross-node 1 GiB live migration": {
			deploy:         freezerDeployment("node-migration-1gib", "default", 1024, liveMigrateAnnotation("freezer")),
			svc:            testService(8080),
			sameNode:       false,
			liveMigration:  true,
			migrationCount: 1,
		},
		"same-node non-live migration": {
			deploy:         freezerDeployment("same-node-migration", "default", 1, migrateAnnotation("freezer"), scaleDownAfter(time.Second)),
			svc:            testService(8080),
			sameNode:       true,
			liveMigration:  false,
			migrationCount: 1,
			afterMigration: nonLiveAfterMigration,
		},
		"cross-node non-live migration": {
			deploy:         freezerDeployment("cross-node-migration", "default", 1, migrateAnnotation("freezer"), scaleDownAfter(time.Second)),
			svc:            testService(8080),
			sameNode:       false,
			liveMigration:  false,
			migrationCount: 1,
			afterMigration: nonLiveAfterMigration,
		},
		"data migration disabled": {
			deploy:                freezerDeployment("data-migration-disabled", "default", 1, liveMigrateAnnotation("freezer"), disableDataMigration()),
			svc:                   testService(8080),
			sameNode:              true,
			liveMigration:         true,
			expectDataNotMigrated: true,
			migrationCount:        1,
		},
	}

	migrate := func(t *testing.T, ctx context.Context, e2e *e2eConfig, tc testCase) {
		pods := podsOfDeployment(t, e2e.client, tc.deploy)
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
		if !tc.liveMigration {
			waitUntilScaledDown(t, ctx, e2e.client, &pod)
		}
		require.NoError(t, e2e.client.Delete(ctx, &pod))
		assert.Eventually(t, func() bool {
			pods := podsOfDeployment(t, e2e.client, tc.deploy)
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
			if !tc.liveMigration {
				require.False(t, migration.Spec.LiveMigration)
			}
			if migration.Status.Containers[0].Condition.Phase == v1.MigrationPhaseFailed {
				printContainerdLogs(t, "zeropod-e2e-worker", "zeropod-e2e-worker2")
			}
			require.NotEqual(t, v1.MigrationPhaseFailed, migration.Status.Containers[0].Condition.Phase)
			t.Logf("migration phase: %s", migration.Status.Containers[0].Condition.Phase)
			return pods[0].Status.Phase == corev1.PodRunning &&
				migration.Status.Containers[0].Condition.Phase == v1.MigrationPhaseCompleted
		}, time.Minute*2, time.Second)

		waitForService(t, ctx, e2e.client, tc.svc, 1)
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.svc == nil {
				tc.svc = testService(defaultTargetPort)
			}
			if tc.beforeMigration == nil {
				tc.beforeMigration = defaultBeforeMigration
			}
			if tc.afterMigration == nil {
				tc.afterMigration = defaultAfterMigration
			}

			cleanupPod := createDeployAndWait(t, ctx, e2e.client, tc.deploy)
			cleanupService := createServiceAndWait(t, ctx, e2e.client, tc.svc, 1)
			defer cleanupPod()
			defer cleanupService()

			for range tc.migrationCount {
				tc.beforeMigration(t)
				checkCtx, cancel := context.WithCancel(ctx)
				writePodData(t, migrationPod(t, tc.deploy))
				defer cancel()
				if tc.liveMigration {
					go func() {
						downtime := availabilityCheck(checkCtx, e2e.port)
						t.Logf("downtime was: %s", downtime)
					}()
				}
				migrate(t, ctx, e2e, tc)
				cancel()
				tc.afterMigration(t, tc)
				if tc.expectDataNotMigrated {
					_, err := readPodData(t, migrationPod(t, tc.deploy))
					assert.Error(t, err)
				} else {
					data, err := readPodDataEventually(t, migrationPod(t, tc.deploy))
					assert.NoError(t, err)
					assert.Equal(t, t.Name(), strings.TrimSpace(data))
				}
			}
		})
	}
}

func printContainerdLogs(t testing.TB, nodes ...string) {
	for _, node := range nodes {
		commandArgs := []string{"exec", node, "journalctl", "-u", "containerd"}
		out, err := exec.Command("docker", commandArgs...).CombinedOutput()
		if err != nil {
			t.Logf("error getting containerd logs: %s", err)
		}
		t.Logf("node %s containerd logs: %s", node, out)
	}

}

func migrationPod(t testing.TB, deploy *appsv1.Deployment) *corev1.Pod {
	pods := podsOfDeployment(t, e2e.client, deploy)
	if len(pods) != 1 {
		t.Errorf("pod of deployment %s not found", deploy.Name)
	}
	return &pods[0]
}

func writePodData(t testing.TB, pod *corev1.Pod) {
	assert.Eventually(t, func() bool {
		_, _, err := podExec(e2e.cfg, pod, fmt.Sprintf("echo %s > /containerdata", t.Name()))
		return err == nil
	}, time.Second*10, time.Second)
}

func readPodData(t testing.TB, pod *corev1.Pod) (string, error) {
	data, _, err := podExec(e2e.cfg, pod, "cat /containerdata")
	return data, err
}

func readPodDataEventually(t testing.TB, pod *corev1.Pod) (string, error) {
	var data string
	var err error
	if !assert.Eventually(t, func() bool {
		data, err = readPodData(t, pod)
		return err == nil
	}, time.Second*10, time.Second) {
		return "", err
	}
	return data, nil
}

func defaultBeforeMigration(t *testing.T) {
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.NoError(c, freezerWrite(t.Name(), e2e.port))
		f, err := freezerRead(e2e.port)
		if assert.NoError(c, err) {
			assert.Equal(c, t.Name(), f.Data)
		}
	}, time.Second*10, time.Second)
}

func defaultAfterMigration(t *testing.T, _ testCase) {
	var f *freeze
	var err error
	if !assert.Eventually(t, func() bool {
		// allow read errors just after migration as for a short time the k8s
		// endpoints might not be available.
		f, err = freezerRead(e2e.port)
		return err == nil
	}, time.Second*5, time.Second) {
		t.Error(err)
		return
	}
	t.Logf("freeze duration: %s", f.LastFreezeDuration)
	assert.Equal(t, t.Name(), f.Data, "freezer memory has persisted migration")
	assert.Less(t, f.LastFreezeDuration, time.Second, "freeze duration")
}

func nonLiveAfterMigration(t *testing.T, tc testCase) {
	require.Never(t, func() bool {
		pods := podsOfDeployment(t, e2e.client, tc.deploy)
		if len(pods) == 0 {
			return true
		}
		status, ok := pods[0].Labels[path.Join(manager.StatusLabelKeyPrefix, "freezer")]
		if !ok {
			return false
		}
		return status != shimv1.ContainerPhase_SCALED_DOWN.String()
	}, time.Second*30, time.Second, "container is scaled down after migration")
	defaultAfterMigration(t, tc)
}
