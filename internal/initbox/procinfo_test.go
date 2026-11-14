package initbox

import "testing"

func TestParseStatus(t *testing.T) {
	var pi pinfo
	parseStatus([]byte("Name:\tdeno\nVmRSS:\t  2048 kB\nRssAnon:\t1024 kB\nThreads:\t7\n"), &pi)
	parseStat([]byte("42 (a) b) c) S 1 2 3 4 5 6 7 8 9 10 150 50 0"), &pi)
	if pi.vmRSS != 2<<20 || pi.private != 1<<20 || !pi.hasPrivate || pi.threads != 7 || pi.cpu.Milliseconds() != 2000 {
		t.Fatalf("%+v", pi)
	}
}

// FuzzProcInfo: /proc/<pid>/status and stat of the scripts (comm is chosen by the script) – no crash, no
// negative sizes.
func FuzzProcInfo(f *testing.F) {
	f.Add([]byte("VmRSS: 10 kB\nThreads: 3"), []byte("1 (x) S 1 1 1 1 1 1 1 1 1 1 5 5"))
	f.Add([]byte("VmRSS: -9223372036854775808 kB"), []byte(")))"))
	f.Fuzz(func(t *testing.T, status, stat []byte) {
		var pi pinfo
		parseStatus(status, &pi)
		parseStat(stat, &pi)
		if pi.vmRSS < 0 || pi.private < 0 {
			t.Fatalf("%+v", pi)
		}
	})
}
