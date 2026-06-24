// Package v1 contains API Schema definitions for the node v1 API group.
// +groupName=runtime.zeropod.ctrox.dev
// +versionName=v1
package v1

import (
	reflect "reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group   = "runtime.zeropod.ctrox.dev"
	Version = "v1"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

var (
	MigrationKind             = reflect.TypeFor[Migration]().Name()
	MigrationGroupKind        = schema.GroupKind{Group: Group, Kind: MigrationKind}.String()
	MigrationKindAPIVersion   = MigrationKind + "." + GroupVersion.String()
	MigrationGroupVersionKind = GroupVersion.WithKind(MigrationKind)
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&Migration{},
		&MigrationList{},
	)

	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
