package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const runtimeClassName = "zeropod"

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test")
	}

	_, client, port := setup(t)
	ctx := context.Background()

	pod := testPod(false, 0)
	svc := testService()
	createPodAndWait(t, ctx, client, pod)
	createServiceAndWait(t, ctx, client, svc, 1)

	before := time.Now()
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d", port))
	assert.NoError(t, err)
	t.Logf("request took %s", time.Since(before))

	assert.NoError(t, err)
	assert.Equal(t, resp.StatusCode, http.StatusOK)
}
