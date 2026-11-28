package tsbox

import "testing"

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
