//go:build generate
// +build generate

// Remove existing CRDs
//go:generate rm -rf ../package/crds

// Generate deepcopy methodsets and CRD manifests
//go:generate go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen  object:headerFile="../../hack/boilerplate.go.txt" paths=./... crd:crdVersions=v1 output:artifacts:config=../../config/crds

// Package runtime contains API types for all runtime related resources.
package runtime

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen" //nolint:typecheck
)
