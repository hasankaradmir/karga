//go:build integration

package docker

import (
	"context"
	"testing"

	"github.com/hasankaradmir/karga/internal/ports/runtimetest"
)

func TestContract(t *testing.T) {
	rt, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	runtimetest.Run(t, rt, runtimetest.Config{
		Image:          "busybox:1.37",
		SleepCommand:   []string{"sleep", "300"},
		HealthyCommand: []string{"true"},
		ReportsIPs:     true,
	})
}
