package v1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MigrationServer struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (ms MigrationServer) Address() string {
	return fmt.Sprintf("%s:%d", ms.Host, ms.Port)
}

// +kubebuilder:object:generate:=true
type MigrationSpec struct {
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
)

type MigrationFailureReason string

const (
	MigrationFailedUnclaimed MigrationFailureReason = "Unclaimed"
)

type MigrationCondition struct {
	Phase  MigrationPhase         `json:"phase,omitempty"`
	Reason MigrationFailureReason `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.containers[*].condition.phase"
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
