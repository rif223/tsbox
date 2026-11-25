package tsbox

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rif223/tsbox/internal/driver"
)

// A box without a driver is refused: there is no default driver; "auto" is unknown.
func TestDriverRequired(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.Create(context.Background(), one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Driver = "" })); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("no driver: %v", err)
	}
	if _, err := e.Create(context.Background(), one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Driver = "auto" })); err == nil || !strings.Contains(err.Error(), "unknown driver") {
		t.Fatalf("auto: %v", err)
	}
	if err := (BoxSpec{Name: "x", Scripts: []ScriptSpec{{Name: "main", Entry: "a.ts"}}}).Validate(); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("validate: %v", err)
	}
}

// A dev box next to a box of the tested driver (with an isolating one: mixed drivers in one engine).
func TestDriverPerBoxDev(t *testing.T) {
	e, _ := newEngine(t)
	st := runToEnd(t, e, one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Driver = "dev" }), time.Minute)
	if st.Driver != "dev" || st.Isolated {
		t.Fatalf("status %+v", st)
	}
	if ex := lastExit(t, st); ex.Reason != ReasonExit || ex.Code != 0 {
		t.Fatalf("exit %+v", ex)
	}
	if isolatedDriver() {
		st := runToEnd(t, e, one(t, "../examples/hello.ts", nil), time.Minute)
		if st.Driver != driverName() || !st.Isolated || lastExit(t, st).Code != 0 {
			t.Fatalf("%s box next to a dev box: %+v", driverName(), st)
		}
	}
}

// Both isolating drivers in one engine at the same time (needs runsc and KVM; the firecracker CI job
// installs runsc for this).
func TestDriverPerBoxIsolatedPair(t *testing.T) {
	needIsolation(t)
	e, _ := newEngine(t)
	other := map[string]string{"gvisor": "firecracker", "firecracker": "gvisor"}[driverName()]
	for _, d := range e.Drivers() {
		if d.Name == other && !d.Available {
			t.Skipf("%s not usable here: %s", other, d.Error)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := e.Run(ctx, one(t, "scripts/service.ts", func(s *BoxSpec) { s.Driver = other }))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Run(ctx, one(t, "scripts/service.ts", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []*Box{a, b} {
		if st := x.Status(); st.State != BoxRunning || !st.Isolated {
			t.Errorf("%s: %+v", x.Spec().Driver, st)
		}
	}
	if a.Status().Driver != other || b.Status().Driver != driverName() {
		t.Errorf("drivers: %s, %s", a.Status().Driver, b.Status().Driver)
	}
	_ = a.Stop(ctx, time.Second)
	_ = b.Stop(ctx, time.Second)
}

// A driver that cannot be loaded fails only the box that asks for it.
func TestDriverUnavailable(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("gvisor and firecracker may be usable here")
	}
	e, _ := newEngine(t)
	_, err := e.Create(context.Background(), one(t, "../examples/hello.ts", func(s *BoxSpec) { s.Driver = "gvisor" }))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("gvisor box: %v", err)
	}
	if st := runToEnd(t, e, one(t, "../examples/hello.ts", nil), time.Minute); lastExit(t, st).Code != 0 {
		t.Fatalf("box after the failed one: %+v", st)
	}
	var found bool
	for _, d := range e.Drivers() {
		if d.Name == driverName() {
			found = d.Available
		}
	}
	if !found {
		t.Errorf("drivers: %+v", e.Drivers())
	}
}

func TestDriverValidation(t *testing.T) {
	s := BoxSpec{Name: "x", Driver: "docker", Scripts: []ScriptSpec{{Name: "main", Entry: "a.ts"}}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "unknown driver") {
		t.Fatalf("unknown driver: %v", err)
	}
}

type fakeDriver struct{}

func (fakeDriver) Name() string   { return "fake" }
func (fakeDriver) Isolated() bool { return true }
func (fakeDriver) Start(context.Context, driver.Spec) (driver.Box, error) {
	return nil, errors.New("fake")
}

// With required isolation, a box cannot ask for the dev driver; the failure is remembered for a while.
func TestDriverRequireIsolation(t *testing.T) {
	e := &Engine{cfg: config{requireIsolation: true},
		drivers: map[string]driver.Driver{"fake": fakeDriver{}}, drvErr: map[string]driverErr{}}
	if d, err := e.driverFor("fake"); err != nil || d.Name() != "fake" {
		t.Fatalf("fake: %v %v", d, err)
	}
	if _, err := e.driverFor(""); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("no driver: %v", err)
	}
	if _, err := e.driverFor("dev"); !errors.Is(err, ErrNotIsolated) {
		t.Fatalf("dev: %v", err)
	}
	if _, ok := e.drvErr["dev"]; !ok {
		t.Fatal("failure not remembered")
	}
	if _, err := e.driverFor("dev"); !errors.Is(err, ErrNotIsolated) {
		t.Fatalf("dev again: %v", err)
	}
}
