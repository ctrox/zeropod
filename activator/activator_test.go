package activator

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivator(t *testing.T) {
	ctx := context.Background()
	netNS, err := ns.GetCurrentNS()
	require.NoError(t, err)

	port := 8080
	s, err := NewServer(ctx, uint16(port), netNS)
	require.NoError(t, err)

	response := "ok"
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))

	if err := s.Start(ctx,
		func() error {
			return nil

		},
		func() (*runc.Container, error) {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				log.Fatal(err)
			}

			// NewUnstartedServer creates a listener. Close that listener and replace
			// with the one we created.
			ts.Listener.Close()
			ts.Listener = l
			ts.Start()

			t.Cleanup(func() {
				ts.Close()
			})

			return nil, nil
		},
		func(c *runc.Container) error {
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	defer s.Stop(ctx)

	c := &http.Client{Timeout: time.Second * 10}

	resp, err := c.Get("http://localhost:8080")
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, response, string(b))
}
