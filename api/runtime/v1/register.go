// Package v1 contains API Schema definitions for the node v1 API group.
// +groupName=runtime.zeropod.ctrox.dev
// +versionName=v1
package v1

import (
	reflect "reflect"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	Group   = "runtime.zeropod.ctrox.dev"
	Version = "v1"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

var (
	MigrationKind             = reflect.TypeOf(Migration{}).Name()
	MigrationGroupKind        = schema.GroupKind{Group: Group, Kind: MigrationKind}.String()
	MigrationKindAPIVersion   = MigrationKind + "." + GroupVersion.String()
	MigrationGroupVersionKind = GroupVersion.WithKind(MigrationKind)
)

func init() {
	SchemeBuilder.Register(&Migration{}, &MigrationList{})
}
