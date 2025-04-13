package shim

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListeningPorts(t *testing.T) {
	ts := httptest.NewServer(nil)
	ts2 := httptest.NewServer(nil)
	ports, err := listeningPortsDeep(os.Getpid())
	require.NoError(t, err)

	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	u2, err := url.Parse(ts2.URL)
	require.NoError(t, err)

	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	port2, err := strconv.Atoi(u2.Port())
	require.NoError(t, err)
	assert.Contains(t, ports, uint16(port))
	assert.Contains(t, ports, uint16(port2))
}
