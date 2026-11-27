package tsbox

import (
	"sort"
	"sync"
)

// SDKCallCount counts finished SDK calls of one box by SDK, method and result ("ok" or an error code such
// as rate_limited, permission_denied, timeout). Calls to SDKs or methods the box does not have are counted
// under SDK "_unknown" (their names come from the script and would be unbounded).
type SDKCallCount struct {
	Box    string `json:"box"`
	SDK    string `json:"sdk"`
	Method string `json:"method"`
	Code   string `json:"code"`
	Count  uint64 `json:"count"`
}

type callKey struct{ box, sdk, method, code string }

type callCounter struct {
	mu sync.Mutex
	m  map[callKey]uint64
}

func (c *callCounter) add(box, sdk, method, code string) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[callKey]uint64{}
	}
	c.m[callKey{box, sdk, method, code}]++
	c.mu.Unlock()
}

func (c *callCounter) forget(box string) {
	c.mu.Lock()
	for k := range c.m {
		if k.box == box {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// SDKCalls returns the call counters of all boxes (since the engine started; a removed box loses its
// counters), sorted.
func (e *Engine) SDKCalls() []SDKCallCount {
	e.calls.mu.Lock()
	out := make([]SDKCallCount, 0, len(e.calls.m))
	for k, n := range e.calls.m {
		out = append(out, SDKCallCount{Box: k.box, SDK: k.sdk, Method: k.method, Code: k.code, Count: n})
	}
	e.calls.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Box != b.Box {
			return a.Box < b.Box
		}
		if a.SDK != b.SDK {
			return a.SDK < b.SDK
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Code < b.Code
	})
	return out
}
