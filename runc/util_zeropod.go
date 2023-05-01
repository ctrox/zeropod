package runc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/pkg/cri/annotations"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// IsSandboxContainer parses the bundle and checks for the
// annotations.ContainerType to see if the container is an internal sandbox
// container.
func IsSandboxContainer(bundlePath string) (bool, error) {
	spec, err := GetSpec(bundlePath)
	if err != nil {
		return false, err
	}

	if v, ok := spec.Annotations[annotations.ContainerType]; ok {
		return v == annotations.ContainerTypeSandbox, nil
	}

	return false, nil
}

func GetSpec(bundlePath string) (*specs.Spec, error) {
	var bundleSpec specs.Spec
	bundleConfigContents, err := os.ReadFile(filepath.Join(bundlePath, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to read budle: %w", err)
	}

	if err := json.Unmarshal(bundleConfigContents, &bundleSpec); err != nil {
		return nil, err
	}

	return &bundleSpec, nil
}

// GetNetworkNS reads the bundle's OCI spec and returns the network NS path of
// the container.
func GetNetworkNS(ctx context.Context, bundlePath string) (string, error) {
	var bundleSpec specs.Spec
	bundleConfigContents, err := os.ReadFile(filepath.Join(bundlePath, "config.json"))
	if err != nil {
		return "", fmt.Errorf("failed to read budle: %w", err)
	}

	if err := json.Unmarshal(bundleConfigContents, &bundleSpec); err != nil {
		return "", err
	}

	for _, ns := range bundleSpec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			return ns.Path, nil
		}
	}

	return "", fmt.Errorf("could not find network namespace in container bundle %s", bundlePath)
}
