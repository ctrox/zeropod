package shim

import (
	"testing"
)

// func TestListeningPorts(t *testing.T) {
// 	ts := httptest.NewServer(nil)

// 	ports, err := ListeningPorts(os.Getpid())
// 	require.NoError(t, err)

// 	u, err := url.Parse(ts.URL)
// 	require.NoError(t, err)

// 	port, err := strconv.ParseUint(u.Port(), 10, 16)
// 	require.NoError(t, err)

// 	assert.Contains(t, ports, uint16(port))
// }

func TestInodes(t *testing.T) {
	inodes(14819)
}
