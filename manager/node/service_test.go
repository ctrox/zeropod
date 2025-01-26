package node

import (
	"testing"

	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
