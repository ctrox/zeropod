package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrox/zeropod/activator"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetinfo(t *testing.T) {
	for name, tc := range map[string]struct {
		id                string
		expectedListeners activator.Listeners
	}{
		"privileged": {
			id: "privileged",
			expectedListeners: activator.Listeners{
				{
					Port:    80,
					Network: activator.NetworkTCP4,
					UID:     0,
				},
				{
					Port:    80,
					Network: activator.NetworkTCP6ONLY,
					UID:     0,
				},
			},
		},
		"unprivileged": {
			id: "unprivileged",
			expectedListeners: activator.Listeners{
				{
					Port:    8080,
					Network: activator.NetworkTCP4,
					UID:     101,
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pwd, err := os.Getwd()
			require.NoError(t, err)
			basePath = new(filepath.Join(pwd, "testdata"))
			containerID = new(tc.id)
			main()
			listeners := decode(t, *containerID)
			assert.Equal(t, tc.expectedListeners, listeners)
		})
	}
}

func decode(t *testing.T, id string) activator.Listeners {
	f, err := os.Open(nodev1.ListenersFile(id))
	require.NoError(t, err)
	//nolint:errcheck
	defer f.Close()
	listeners := activator.Listeners{}
	err = json.NewDecoder(f).Decode(&listeners)
	assert.NoError(t, err)
	return listeners
}
