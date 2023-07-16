package zeropod

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

	ports, err := ListeningPorts(os.Getpid())
	require.NoError(t, err)

	u, err := url.Parse(ts.URL)
	require.NoError(t, err)

	port, err := strconv.ParseUint(u.Port(), 10, 16)
	require.NoError(t, err)

	assert.Contains(t, ports, uint16(port))
}
