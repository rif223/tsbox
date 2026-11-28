package tsbox

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rif223/tsbox/internal/driver"
	"github.com/rif223/tsbox/manifest"
)

// ErrQuotaExceeded: a tenant would exceed its quota (WithTenantQuotas).
var ErrQuotaExceeded = errors.New("quota exceeded")

// Quota limits what the boxes of one tenant may use together. 0 means unlimited. Boxes counts all boxes
// the engine knows (also stopped ones); Memory, CPU and PIDs add up the box limits of running boxes.
type Quota struct {
	Boxes  int     `json:"boxes,omitempty"`
	Memory int64   `json:"memory,omitempty"`
	CPU    float64 `json:"cpu,omitempty"`
	PIDs   int     `json:"pids,omitempty"`
}

// WithTenantQuotas groups boxes by the value of a label (e.g. "tenant") and limits every group with the
// quota fn returns for it. Boxes without the label have no quota. Create checks Boxes, Start and Update
// check the resources.
func WithTenantQuotas(label string, fn func(tenant string) Quota) Option {
	return func(c *config) { c.quotaLabel, c.quotaFn = label, fn }
}

// ParseQuota reads "boxes=10,memory=2Gi,cpu=4,pids=1024" (all parts optional).
func ParseQuota(s string) (Quota, error) {
	var q Quota
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return q, fmt.Errorf("quota %q: expected key=value", part)
		}
		var err error
		switch k {
		case "boxes":
			q.Boxes, err = strconv.Atoi(v)
		case "memory":
			q.Memory, err = manifest.ParseBytes(v)
		case "cpu":
			q.CPU, err = strconv.ParseFloat(v, 64)
		case "pids":
			q.PIDs, err = strconv.Atoi(v)
		default:
			return q, fmt.Errorf("quota: unknown key %q (boxes, memory, cpu, pids)", k)
		}
		if err != nil {
			return q, fmt.Errorf("quota %s: %w", k, err)
		}
	}
	if q.Boxes < 0 || q.Memory < 0 || q.CPU < 0 || q.PIDs < 0 {
		return q, errors.New("quota: negative value")
	}
	if math.IsNaN(q.CPU) || math.IsInf(q.CPU, 0) { // NaN would mean "unlimited": every comparison fails
		return q, errors.New("quota cpu: not a finite number")
	}
	return q, nil
}

// QuotaLabel is the label of WithTenantQuotas ("" without quotas); QuotaFor returns a tenant's quota.
func (e *Engine) QuotaLabel() string { return e.cfg.quotaLabel }

func (e *Engine) QuotaFor(tenant string) Quota {
	if e.cfg.quotaFn == nil || tenant == "" {
		return Quota{}
	}
	return e.cfg.quotaFn(tenant)
}

// TenantUsage is what the boxes of a tenant use.
type TenantUsage struct {
	Tenant  string `json:"tenant"`
	Boxes   int    `json:"boxes"`
	Running int    `json:"running"`
	Limits  Limits `json:"limits"` // sums of the running boxes
	Quota   Quota  `json:"quota"`
}

// Tenants reports usage and quota of every tenant (empty without WithTenantQuotas).
func (e *Engine) Tenants() []TenantUsage {
	if e.cfg.quotaFn == nil {
		return nil
	}
	by := map[string]*TenantUsage{}
	var order []string
	e.mu.Lock()
	boxes := make([]*Box, 0, len(e.boxes))
	for _, b := range e.boxes {
		boxes = append(boxes, b)
	}
	e.mu.Unlock()
	for _, b := range boxes {
		t := b.spec.Labels[e.cfg.quotaLabel]
		if t == "" {
			continue
		}
		u := by[t]
		if u == nil {
			u = &TenantUsage{Tenant: t, Quota: e.cfg.quotaFn(t)}
			by[t] = u
			order = append(order, t)
		}
		u.Boxes++
		if !b.terminal() {
			u.Running++
			u.Limits.Memory += b.spec.Limits.Memory
			u.Limits.CPU += b.spec.Limits.CPU
			u.Limits.PIDs += b.spec.Limits.PIDs
		}
	}
	out := make([]TenantUsage, 0, len(order))
	for _, t := range order {
		out = append(out, *by[t])
	}
	return out
}

// checkQuota checks a specification against its tenant's quota: the number of boxes (create) or the
// resources of running boxes (resources). The box with the same name does not count (Update, restart).
func (e *Engine) checkQuota(spec BoxSpec, resources bool) error {
	if e.cfg.quotaFn == nil {
		return nil
	}
	tenant := spec.Labels[e.cfg.quotaLabel]
	if tenant == "" {
		return nil
	}
	q := e.cfg.quotaFn(tenant)
	if !resources && q.Boxes == 0 {
		return nil
	}
	if !resources {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.boxQuotaLocked(spec)
	}
	ns, err := normalize(spec)
	if err != nil {
		return err
	}
	sum := ns.Limits
	e.mu.Lock()
	others := make([]*Box, 0, len(e.boxes))
	for name, b := range e.boxes {
		if name != spec.Name && b.spec.Labels[e.cfg.quotaLabel] == tenant {
			others = append(others, b)
		}
	}
	e.mu.Unlock()
	for _, b := range others {
		if !b.terminal() {
			sum.Memory += b.spec.Limits.Memory
			sum.CPU += b.spec.Limits.CPU
			sum.PIDs += b.spec.Limits.PIDs
		}
	}
	switch {
	case q.Memory > 0 && sum.Memory > q.Memory:
		return fmt.Errorf("%w: tenant %q: running boxes would use %s of %s memory", ErrQuotaExceeded, tenant,
			driver.FormatBytes(sum.Memory), driver.FormatBytes(q.Memory))
	case q.CPU > 0 && sum.CPU > q.CPU+1e-9:
		return fmt.Errorf("%w: tenant %q: running boxes would use %g of %g CPU cores", ErrQuotaExceeded, tenant, sum.CPU, q.CPU)
	case q.PIDs > 0 && sum.PIDs > q.PIDs:
		return fmt.Errorf("%w: tenant %q: running boxes would use %d of %d PIDs", ErrQuotaExceeded, tenant, sum.PIDs, q.PIDs)
	}
	return nil
}

// boxQuotaLocked checks the number of boxes of spec's tenant (e.mu held, so that Create counts and adds the
// box in one step). The box with the same name does not count.
func (e *Engine) boxQuotaLocked(spec BoxSpec) error {
	if e.cfg.quotaFn == nil {
		return nil
	}
	tenant := spec.Labels[e.cfg.quotaLabel]
	if tenant == "" {
		return nil
	}
	q := e.cfg.quotaFn(tenant)
	if q.Boxes == 0 {
		return nil
	}
	n := 1
	for name, b := range e.boxes {
		if name != spec.Name && b.spec.Labels[e.cfg.quotaLabel] == tenant {
			n++
		}
	}
	if n > q.Boxes {
		return fmt.Errorf("%w: tenant %q may have %d boxes", ErrQuotaExceeded, tenant, q.Boxes)
	}
	return nil
}
