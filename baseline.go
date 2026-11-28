package tsbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/rif223/tsbox/internal/fsutil"
)

// maxBaselineRSS bounds the value init reports (an empty Deno uses about 110 MiB VmRSS under gVisor).
const maxBaselineRSS = 1 << 30

// baselines remembers the VmRSS of an empty Deno per driver, rootfs fingerprint and init binary (gVisor has
// no RssAnon: init subtracts the value from VmRSS). Measuring it takes init about a second before hello, so
// only the first start of a combination measures; later starts (also after a daemon restart: the values are
// kept in <state>/baseline-rss.json) pass it in driver.Spec.BaselineRSS.
type baselines struct {
	mu     sync.Mutex
	loaded bool
	m      map[string]int64
}

// baselineKey identifies what the value depends on; "" if there is nothing to remember (no profile
// fingerprint: dev and firecracker measure RSS without a baseline).
func (e *Engine) baselineKey(drv, profileHash string) string {
	if profileHash == "" {
		return ""
	}
	st, err := os.Stat(e.cfg.init)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d-%d", drv, profileHash, st.Size(), st.ModTime().UnixNano())
}

func (e *Engine) baselinePath() string { return filepath.Join(e.cfg.stateDir, "baseline-rss.json") }

// baselineRSS returns the remembered value of key (0 = unknown).
func (e *Engine) baselineRSS(key string) int64 {
	if key == "" {
		return 0
	}
	e.base.mu.Lock()
	defer e.base.mu.Unlock()
	e.loadBaselinesLocked()
	return e.base.m[key]
}

// rememberBaselineRSS stores the value of a hello (ignored if outside (0, maxBaselineRSS]).
func (e *Engine) rememberBaselineRSS(key string, v int64) {
	if key == "" || v <= 0 || v > maxBaselineRSS {
		return
	}
	e.base.mu.Lock()
	defer e.base.mu.Unlock()
	e.loadBaselinesLocked()
	if e.base.m[key] == v {
		return
	}
	e.base.m[key] = v
	b, err := json.Marshal(e.base.m)
	if err != nil {
		return
	}
	tmp := e.baselinePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = fsutil.Rename(tmp, e.baselinePath())
	}
}

func (e *Engine) loadBaselinesLocked() {
	if e.base.loaded {
		return
	}
	e.base.loaded, e.base.m = true, map[string]int64{}
	b, err := os.ReadFile(e.baselinePath())
	if err != nil {
		return
	}
	var m map[string]int64
	if json.Unmarshal(b, &m) != nil {
		return // damaged: measure again
	}
	for k, v := range m {
		if v > 0 && v <= maxBaselineRSS && len(m) <= 256 {
			e.base.m[k] = v
		}
	}
}
