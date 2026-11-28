package tsbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrUnknownBox: the engine does not know the box.
var ErrUnknownBox = errors.New("box not found")

// Update replaces the specification of an existing box (new scripts, SDKs, limits …) and keeps its name,
// storage and log files. Everything is built and checked first while the old box keeps running; only then
// is the old box stopped (with grace, see Stop) and the new one started, if the old one was running. A build
// or check error leaves the old box untouched. If the new box cannot start, Update tries to start the
// old specification again and returns the error.
func (e *Engine) Update(ctx context.Context, spec BoxSpec, grace time.Duration) (*Box, error) {
	old, ok := e.Box(spec.Name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBox, spec.Name)
	}
	// Build into the shared artifact store without touching the running box; Create below finds the
	// artifacts in the build cache.
	if _, err := e.Check(ctx, spec); err != nil {
		return nil, err
	}
	if err := e.checkQuota(spec, false); err != nil {
		return nil, err
	}
	wasRunning := !old.terminal()
	// The resources too, before the old box stops: otherwise Start below fails on the quota and the old
	// box only comes back through the rollback (a restart). The box itself is not counted (same name).
	if wasRunning {
		if err := e.checkQuota(spec, true); err != nil {
			return nil, err
		}
	}
	oldSpec := old.spec // with the Env of the caller that created it
	if wasRunning {
		if err := old.Stop(ctx, grace); err != nil {
			return nil, err
		}
	}
	nb, err := e.Create(ctx, spec)
	if err == nil && wasRunning {
		if err = nb.Start(ctx); err != nil {
			e.forget(spec.Name, nb)
		}
	}
	if err == nil {
		return nb, nil
	}
	// Roll back: the old specification built before, so it builds from the cache now.
	bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if rb, rerr := e.Create(bg, oldSpec); rerr == nil {
		if wasRunning {
			if rerr = rb.Start(bg); rerr != nil {
				e.forget(oldSpec.Name, rb)
			}
		}
		if rerr != nil {
			err = fmt.Errorf("%w (rolling back to the old specification failed too: %v)", err, rerr)
		}
	} else {
		err = fmt.Errorf("%w (rolling back to the old specification failed too: %v)", err, rerr)
	}
	return nil, err
}
