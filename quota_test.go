package tsbox

import (
	"math"
	"testing"
)

func TestParseQuota(t *testing.T) {
	q, err := ParseQuota("boxes=10, memory=2Gi,cpu=1.5,pids=512")
	if err != nil || q != (Quota{Boxes: 10, Memory: 2 << 30, CPU: 1.5, PIDs: 512}) {
		t.Fatalf("%+v %v", q, err)
	}
	if q, err := ParseQuota(""); err != nil || q != (Quota{}) {
		t.Fatalf("empty: %+v %v", q, err)
	}
	for _, bad := range []string{"boxes", "ram=1Gi", "cpu=-1", "memory=lots", "cpu=NaN", "cpu=inf", "cpu=-Inf"} {
		if _, err := ParseQuota(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// FuzzParseQuota: no input crashes ParseQuota; an accepted quota has no negative or non-finite value.
func FuzzParseQuota(f *testing.F) {
	for _, s := range []string{"boxes=10,memory=2Gi,cpu=4,pids=1024", "", "cpu=NaN", "memory=-1", "=,=", "boxes=1e9"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := ParseQuota(s)
		if err != nil {
			return
		}
		if q.Boxes < 0 || q.Memory < 0 || q.PIDs < 0 || !(q.CPU >= 0) || math.IsInf(q.CPU, 0) {
			t.Fatalf("%q: %+v", s, q)
		}
	})
}
