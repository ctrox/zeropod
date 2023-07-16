package zeropod

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/runtime-spec/specs-go"
)

const RuntimeName = "io.containerd.zeropod.v2"

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
func GetNetworkNS(spec *specs.Spec) (string, error) {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			return ns.Path, nil
		}
	}

	return "", fmt.Errorf("could not find network namespace in container spec")
}

// GetPIDNS reads the bundle's OCI spec and returns the PID NS path of the
// container.
func GetPIDNS(spec *specs.Spec) (string, error) {
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.PIDNamespace {
			return ns.Path, nil
		}
	}

	return "", fmt.Errorf("could not find pid namespace in container spec")
}
