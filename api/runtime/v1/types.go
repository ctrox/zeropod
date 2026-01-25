package v1

import (
	"net"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MigrationServer struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (ms MigrationServer) Address() string {
	return net.JoinHostPort(ms.Host, strconv.Itoa(ms.Port))
}

// +kubebuilder:object:generate:=true
type MigrationSpec struct {
	// LiveMigration indicates if this migration is done live (lazy) or not. If
	// set, the source node will setup a page server to serve memory pages
	// during live migration. If false, the image copy will include all memory
	// pages, which might result in a slower migration.
	// +optional
	LiveMigration bool `json:"liveMigration"`
	// RestoreReady indicates the shim is up and running and is ready to restore
	// so checkpointing can be started on the other side.
	RestoreReady bool `json:"restoreReady"`
	// SourceNode of the pod to be migrated
	SourceNode string `json:"sourceNode"`
	// TargetNode of the pod to be migrated
	// +optional
	TargetNode string `json:"targetNode,omitempty"`
	// SourcePod of the migration
	// +optional
	SourcePod string `json:"sourcePod,omitempty"`
	// TargetPod of the migration
	// +optional
	TargetPod string `json:"targetPod,omitempty"`
	// PodTemplateHash of the source pod. This is used to find a suitable target
	// pod.
	PodTemplateHash string `json:"podTemplateHash"`
	// Containers to be migrated
	// +listType:=map
	// +listMapKey:=name
	Containers []MigrationContainer `json:"containers"`
}

// +kubebuilder:object:generate:=true
type MigrationContainer struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// ImageServer to pull the CRIU checkpoint image from.
	// +optional
	ImageServer *MigrationServer `json:"imageServer,omitempty"`
	// PageServer to pull the memory pages from during lazy migration.
	// +optional
	PageServer *MigrationServer `json:"pageServer,omitempty"`

	Ports []int32 `json:"ports,omitempty"`
}

// +kubebuilder:object:generate:=true
type MigrationStatus struct {
	// Containers indicates the status of the individual container migrations.
	// +listType:=map
	// +listMapKey:=name
	Containers []MigrationContainerStatus `json:"containers"`
}

// +kubebuilder:object:generate:=true
type MigrationContainerStatus struct {
	Name              string             `json:"name"`
	Condition         MigrationCondition `json:"condition"`
	PausedAt          metav1.MicroTime   `json:"pausedAt,omitempty"`
	RestoredAt        metav1.MicroTime   `json:"restoredAt,omitempty"`
	MigrationDuration metav1.Duration    `json:"migrationDuration,omitempty"`
}

type MigrationPhase string

const (
	MigrationPhasePending   MigrationPhase = "Pending"
	MigrationPhaseRunning   MigrationPhase = "Running"
	MigrationPhaseCompleted MigrationPhase = "Completed"
	MigrationPhaseFailed    MigrationPhase = "Failed"
	MigrationPhaseUnclaimed MigrationPhase = "Unclaimed"
)

// Final returns if the phase is final
func (phase MigrationPhase) Final() bool {
	return phase == MigrationPhaseCompleted ||
		phase == MigrationPhaseFailed ||
		phase == MigrationPhaseUnclaimed
}

type MigrationCondition struct {
	Phase MigrationPhase `json:"phase,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.containers[*].condition.phase"
// +kubebuilder:printcolumn:name="Live",type="boolean",JSONPath=".spec.liveMigration"
// +kubebuilder:printcolumn:name="Duration",type="string",JSONPath=".status.containers[*].migrationDuration"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:scope=Namespaced
// Migration tracks container live migrations done by zeropod.
type Migration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MigrationSpec   `json:"spec,omitempty"`
	Status MigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MigrationList contains a list of Migration.
type MigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Migration `json:"items"`
}

// MatchesPod checks if the migration would be a match for this pod.
func (mig Migration) MatchesPod(pod *corev1.Pod) bool {
	return mig.Spec.PodTemplateHash == pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
}

// Claim sets the target pod and node to claim the migration
func (mig *Migration) Claim(targetPodName, targetNodeName string) {
	mig.Spec.TargetPod = targetPodName
	mig.Spec.TargetNode = targetNodeName
}

// ClaimedAndMatchesPod indicates the migration has been claimed and targets our
// pod.
func (mig Migration) ClaimedAndMatchesPod(pod *corev1.Pod) bool {
	return mig.Spec.TargetPod == pod.Name && mig.Claimed()
}

// Claimed indicates if this migration already has been claimed
func (mig Migration) Claimed() bool {
	// a non-empty target node or a container in status unclaimed indicates the
	// migration has already been claimed before.
	return mig.Spec.TargetNode != "" || mig.hasUnclaimedContainer()
}

func (mig Migration) hasUnclaimedContainer() bool {
	return slices.ContainsFunc(mig.Status.Containers, func(status MigrationContainerStatus) bool {
		return status.Condition.Phase == MigrationPhaseUnclaimed
	})
}
