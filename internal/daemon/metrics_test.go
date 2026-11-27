package daemon

import (
	"bytes"
	"testing"
)

func TestMetricsWriter(t *testing.T) {
	var buf bytes.Buffer
	m := &metricsWriter{w: &buf, seen: map[string]bool{}}
	m.head("x_total", "counter", "Help.")
	m.val("x_total", 3, "box", `a"b\c`+"\n")
	m.head("x_total", "counter", "Help.") // only once
	m.val("x_total", 0.5)
	want := "# HELP x_total Help.\n# TYPE x_total counter\n" + `x_total{box="a\"b\\c\n"} 3` + "\nx_total 0.5\n"
	if buf.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", buf.String(), want)
	}
}
