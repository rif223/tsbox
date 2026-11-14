// Package logfile writes and reads the log files of a box: one file of JSON lines per script
// (<script>.log) and one rotated predecessor (<script>.log.1). Never more than 2 × Max bytes are used per
// script.
package logfile

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

// DefaultMax is the size from which a file is rotated.
const DefaultMax = 10 << 20

// Line is a stored log line. Seq grows strictly monotonically across all scripts of an engine.
type Line struct {
	Seq    int64     `json:"seq"`
	At     time.Time `json:"at"`
	Script string    `json:"script"`
	Level  string    `json:"level"`
	Msg    string    `json:"msg"`
}

// Path is the current file of a script.
func Path(dir, script string) string { return filepath.Join(dir, script+".log") }

// Writer writes the files of a box. It is safe for concurrent use.
type Writer struct {
	dir    string
	max    int64
	mu     sync.Mutex
	files  map[string]*file
	closed bool
}

type file struct {
	f    *os.File
	size int64
}

// NewWriter creates nothing yet; files are created on the first write.
func NewWriter(dir string, max int64) *Writer {
	if max <= 0 {
		max = DefaultMax
	}
	return &Writer{dir: dir, max: max, files: map[string]*file{}}
}

// Write appends a line. After Close nothing is written any more.
func (w *Writer) Write(l Line) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	f, err := w.open(l.Script)
	if err != nil {
		return err
	}
	if f.size > 0 && f.size+int64(len(b)) > w.max {
		w.rotate(l.Script, f)
		if f, err = w.open(l.Script); err != nil {
			return err
		}
	}
	n, err := f.f.Write(b)
	f.size += int64(n)
	return err
}

func (w *Writer) open(script string) (*file, error) {
	if f, ok := w.files[script]; ok {
		return f, nil
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(Path(w.dir, script), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, err
	}
	f := &file{f: fh, size: st.Size()}
	w.files[script] = f
	return f, nil
}

// rotate renames <script>.log to <script>.log.1. If that fails (on Windows, while a reader keeps the
// file open), appending continues and the next write tries again.
func (w *Writer) rotate(script string, f *file) {
	p := Path(w.dir, script)
	f.f.Close()
	delete(w.files, script)
	_ = os.Remove(p + ".1")
	_ = fsutil.Rename(p, p+".1")
}

// Close closes all files; later writes are discarded.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	var errs []error
	for k, f := range w.files {
		errs = append(errs, f.f.Close())
		delete(w.files, k)
	}
	return errors.Join(errs...)
}

// Filter selects lines when reading.
type Filter struct {
	Since    time.Time // only lines from this time on
	Script   string    // only this script ("" = all)
	Tail     int       // only the last n lines (0 = all)
	AfterSeq int64     // only lines with a larger Seq
}

func (f Filter) match(l Line) bool {
	return l.Seq > f.AfterSeq && (f.Since.IsZero() || !l.At.Before(f.Since)) && (f.Script == "" || l.Script == f.Script)
}

// Scripts names the scripts with log files in dir, sorted.
func Scripts(dir string) []string {
	ents, _ := os.ReadDir(dir)
	seen := map[string]bool{}
	for _, e := range ents {
		n := strings.TrimSuffix(e.Name(), ".1")
		if strings.HasSuffix(n, ".log") {
			seen[strings.TrimSuffix(n, ".log")] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Read reads all matching lines in Seq order (merged across all scripts) and calls fn until fn returns
// false. Incomplete or broken lines are skipped. Returns the largest Seq read.
func Read(dir string, f Filter, fn func(Line) bool) (int64, error) {
	scripts := Scripts(dir)
	if f.Script != "" {
		scripts = nil
		for _, s := range Scripts(dir) {
			if s == f.Script {
				scripts = []string{s}
			}
		}
	}
	h := &lineHeap{}
	var readers []*reader
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	for _, s := range scripts {
		p := Path(dir, s)
		r := &reader{paths: []string{p + ".1", p}}
		readers = append(readers, r)
		if l, ok := r.next(); ok {
			heap.Push(h, item{l, r})
		}
	}
	var last int64
	var ring []Line
	for h.Len() > 0 {
		it := heap.Pop(h).(item)
		if l, ok := it.r.next(); ok {
			heap.Push(h, item{l, it.r})
		}
		l := it.l
		last = max(last, l.Seq)
		if !f.match(l) {
			continue
		}
		if f.Tail > 0 {
			ring = append(ring, l)
			if len(ring) > f.Tail {
				ring = ring[1:]
			}
			continue
		}
		if !fn(l) {
			return last, nil
		}
	}
	for _, l := range ring {
		if !fn(l) {
			break
		}
	}
	return last, nil
}

// reader reads several files of a script one after the other.
type reader struct {
	paths []string
	f     *os.File
	sc    *bufio.Scanner
}

func (r *reader) next() (Line, bool) {
	for {
		if r.sc == nil {
			if len(r.paths) == 0 {
				return Line{}, false
			}
			p := r.paths[0]
			r.paths = r.paths[1:]
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			r.f, r.sc = f, bufio.NewScanner(f)
			r.sc.Buffer(make([]byte, 64<<10), 1<<20)
		}
		for r.sc.Scan() {
			var l Line
			if json.Unmarshal(r.sc.Bytes(), &l) == nil && l.Seq > 0 {
				return l, true
			}
		}
		r.close()
	}
}

func (r *reader) close() {
	if r.f != nil {
		r.f.Close()
	}
	r.f, r.sc = nil, nil
}

type item struct {
	l Line
	r *reader
}

type lineHeap []item

func (h lineHeap) Len() int           { return len(h) }
func (h lineHeap) Less(i, j int) bool { return h[i].l.Seq < h[j].l.Seq }
func (h lineHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *lineHeap) Push(x any)        { *h = append(*h, x.(item)) }
func (h *lineHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}
