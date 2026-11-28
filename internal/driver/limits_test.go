package driver

import (
	"math"
	"testing"
)

func TestLimitsValidate(t *testing.T) {
	ok := Limits{MemoryBytes: 128 << 20, CPUCores: 0.5, PIDs: 64}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, cpu := range []float64{0, 65, math.NaN(), math.Inf(1), math.Inf(-1)} {
		l := ok
		l.CPUCores = cpu
		if l.Validate() == nil {
			t.Errorf("CPU %g accepted", cpu)
		}
	}
}
