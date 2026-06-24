package node

import (
	"fmt"

	"github.com/checkpoint-restore/go-criu/v7"
	"github.com/checkpoint-restore/go-criu/v7/rpc"
)

func checkLazyPages() error {
	c := criu.MakeCriu()
	feat, err := c.FeatureCheck(&rpc.CriuFeatures{LazyPages: new(true)})
	if err != nil {
		return fmt.Errorf("lazy pages feature check failed with: %w", err)
	}
	if feat.LazyPages == nil || !*feat.LazyPages {
		return fmt.Errorf("lazy pages feature not available")
	}
	return nil
}
