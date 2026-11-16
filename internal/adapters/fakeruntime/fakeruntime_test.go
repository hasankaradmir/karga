package fakeruntime

import (
	"testing"

	"github.com/hasankaradmir/karga/internal/ports/runtimetest"
)

func TestContract(t *testing.T) {
	runtimetest.Run(t, New(), runtimetest.Config{
		Image:          "busybox:1.37",
		SleepCommand:   []string{"sleep", "300"},
		HealthyCommand: []string{"true"},
	})
}
