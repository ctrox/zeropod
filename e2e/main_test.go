package e2e

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if e2e != nil {
		if err := e2e.cleanup(); err != nil {
			fmt.Printf("error during test cleanup: %s", err)
			os.Exit(1)
		}
	}
	os.Exit(code)
}
