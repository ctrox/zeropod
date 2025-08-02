package shim

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectProbe(t *testing.T) {
	ctx := context.Background()

	for name, tc := range map[string]struct {
		probeDetected bool
		clientFunc    func(t *testing.T, addr string)
	}{
		"http kube-probe/1.32": {
			probeDetected: true,
			clientFunc:    httpRequest("kube-probe/1.32", http.StatusNoContent, nil),
		},
		"http kube-probe/any": {
			clientFunc:    httpRequest("kube-probe/any", http.StatusNoContent, nil),
			probeDetected: true,
		},
		"tcp probe": {
			clientFunc:    kubeTCPProbe,
			probeDetected: true,
		},
		"http but not a probe": {
			clientFunc:    httpRequest("kube-notprobe/1.32", http.StatusOK, nil),
			probeDetected: false,
		},
		"probe request header bigger than buffer": {
			clientFunc: httpRequest("kube-probe/1.32", http.StatusOK, func(req *http.Request) {
				rnd, err := randomData(defaultProbeBufferSize * 10)
				assert.NoError(t, err)
				req.Header.Set("random-stuff", base64.URLEncoding.EncodeToString(rnd))
			}),
			probeDetected: false,
		},
		"probe request path bigger than buffer": {
			clientFunc: httpRequest("kube-probe/1.32", http.StatusOK, func(req *http.Request) {
				rnd, err := randomData(defaultProbeBufferSize * 10)
				assert.NoError(t, err)
				req.URL.Path = "/" + base64.URLEncoding.EncodeToString(rnd)
			}),
			probeDetected: false,
		},
		"random TCP data": {
			clientFunc:    writeRandomTCPData(10),
			probeDetected: false,
		},
		"random TCP data bigger than buffer": {
			clientFunc:    writeRandomTCPData(defaultProbeBufferSize * 1024),
			probeDetected: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)

			clientDone := make(chan bool)
			go func() {
				tc.clientFunc(t, l.Addr().String())
				clientDone <- true
			}()

			conn, err := l.Accept()
			require.NoError(t, err)
			c := &Container{cfg: &Config{ProbeBufferSize: defaultProbeBufferSize}}
			newConn, cont, err := c.detectProbe(ctx)(conn)
			require.NoError(t, err)
			if cont {
				resp := http.Response{
					StatusCode: http.StatusOK,
				}
				resp.Write(newConn)
			}
			assert.Equal(t, !tc.probeDetected, cont)

			<-clientDone
			newConn.Close()
		})
	}
}

func httpRequest(userAgent string, expectedStatus int, modifyReq func(*http.Request)) func(t *testing.T, addr string) {
	return func(t *testing.T, addr string) {
		// emulate kubernetes HTTP probe:
		// https://github.com/kubernetes/kubernetes/blob/7cc3faf39d89d11c910db9ad19adfd931250e01c/pkg/probe/http/http.go#L48
		req, err := http.NewRequest(http.MethodGet, "http://"+addr, nil)
		if !assert.NoError(t, err) {
			return
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "*/*")
		if modifyReq != nil {
			modifyReq(req)
		}

		c := http.Client{
			Transport: &http.Transport{
				DisableCompression: true,
				DisableKeepAlives:  true,
			},
		}
		resp, err := c.Do(req)
		assert.NoError(t, err)
		assert.Equal(t, expectedStatus, resp.StatusCode)
	}
}

// kubeTCPProbe emulates a TCP probe:
// https://github.com/kubernetes/kubernetes/blob/7cc3faf39d89d11c910db9ad19adfd931250e01c/pkg/probe/tcp/tcp.go#L50
func kubeTCPProbe(t *testing.T, addr string) {
	conn, err := net.Dial("tcp", addr)
	if !assert.NoError(t, err) {
		return
	}
	assert.NoError(t, conn.Close())
}

func writeRandomTCPData(size int) func(t *testing.T, addr string) {
	return func(t *testing.T, addr string) {
		conn, err := net.Dial("tcp", addr)
		if !assert.NoError(t, err) {
			return
		}
		randomData, err := randomData(size)
		if !assert.NoError(t, err) {
			return
		}
		conn.Write(randomData)
	}
}

func randomData(size int) ([]byte, error) {
	randomData := make([]byte, size)
	if _, err := rand.Read(randomData); err != nil {
		return nil, err
	}
	return randomData, nil
}
