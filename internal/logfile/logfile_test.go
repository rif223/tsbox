package logfile

import (
	"os"
	"strings"
	"testing"
	"time"
)

func collect(t *testing.T, dir string, f Filter) []Line {
	t.Helper()
	var out []Line
	if _, err := Read(dir, f, func(l Line) bool { out = append(out, l); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRotationAndMerge(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir, 1000)
	t0 := time.Now()
	msg := strings.Repeat("x", 80)
	for i := 1; i <= 100; i++ {
		s := "a"
		if i%2 == 0 {
			s = "b"
		}
		if err := w.Write(Line{Seq: int64(i), At: t0.Add(time.Duration(i) * time.Millisecond), Script: s, Level: "info", Msg: msg}); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	for _, s := range []string{"a", "b"} {
		for _, p := range []string{Path(dir, s), Path(dir, s) + ".1"} {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() > 1000 {
				t.Fatalf("%s: %d bytes > limit", p, st.Size())
			}
		}
	}
	all := collect(t, dir, Filter{})
	if len(all) == 0 || len(all) >= 100 {
		t.Fatalf("expected fewer than 100 lines after rotation, %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("order: %d after %d", all[i].Seq, all[i-1].Seq)
		}
	}
	if all[len(all)-1].Seq != 100 {
		t.Fatalf("last line %d", all[len(all)-1].Seq)
	}
	tail := collect(t, dir, Filter{Tail: 3, Script: "a"})
	if len(tail) != 3 || tail[2].Seq != 99 || tail[0].Script != "a" {
		t.Fatalf("tail: %+v", tail)
	}
	since := collect(t, dir, Filter{Since: t0.Add(95 * time.Millisecond)})
	if len(since) != 6 {
		t.Fatalf("since: %d lines", len(since))
	}
	after := collect(t, dir, Filter{AfterSeq: 98})
	if len(after) != 2 {
		t.Fatalf("afterSeq: %d", len(after))
	}
	if got := Scripts(dir); len(got) != 2 {
		t.Fatalf("Scripts: %v", got)
	}
}

func TestBrokenLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir, "x"), []byte("{\"seq\":1,\"script\":\"x\",\"msg\":\"ok\"}\nkaputt\n{\"seq\":2,\"scr"), 0o600)
	got := collect(t, dir, Filter{})
	if len(got) != 1 || got[0].Msg != "ok" {
		t.Fatalf("%+v", got)
	}
	w := NewWriter(dir, 0)
	w.Close()
	if err := w.Write(Line{Seq: 9, Script: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(collect(t, dir, Filter{})) != 1 {
		t.Fatal("Write after Close wrote something")
	}
}

// FuzzRead: arbitrary (broken) log files do not crash the reader; it only returns lines with Seq > 0
// that match the filter.
func FuzzRead(f *testing.F) {
	f.Add([]byte(`{"seq":1,"at":"2026-11-02T10:00:00Z","script":"main","level":"info","msg":"a"}`+"\n"), []byte(`{"seq":2}`), 0)
	f.Add([]byte("kaputt\n{\"seq\":-1}\n"), []byte(`{"seq":9007199254740993,"at":"x"}`), 1)
	f.Fuzz(func(t *testing.T, cur, old []byte, tail int) {
		dir := t.TempDir()
		os.WriteFile(Path(dir, "main"), cur, 0o600)
		os.WriteFile(Path(dir, "main")+".1", old, 0o600)
		flt := Filter{Tail: tail % 50, AfterSeq: 0}
		if flt.Tail < 0 {
			flt.Tail = -flt.Tail
		}
		n := 0
		_, err := Read(dir, flt, func(l Line) bool {
			if l.Seq <= 0 || !flt.match(l) {
				t.Fatalf("line %+v", l)
			}
			n++
			return true
		})
		if err != nil {
			return
		}
		if flt.Tail > 0 && n > flt.Tail {
			t.Fatalf("%d lines with Tail %d", n, flt.Tail)
		}
	})
}
