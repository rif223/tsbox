//go:build !linux

// Package firecracker starts every box in its own Firecracker microVM (Linux with KVM only).
package firecracker

import (
	"context"
	"errors"

	"github.com/rif223/tsbox/internal/build"
	"github.com/rif223/tsbox/internal/driver"
)

// KVMDevice is the device Firecracker needs.
const KVMDevice = "/dev/kvm"

var errLinux = errors.New("firecracker: only on Linux with KVM")

// Config of the driver.
type Config struct {
	Firecracker  string
	Kernel       string
	RootFS       string
	StateDir     string
	CgroupParent string
	Jailer       string
	JailUID      int
}

// Driver is not available on this system.
type Driver struct{}

func New(Config) (*Driver, error) { return nil, errLinux }
func CheckKVM() error             { return errLinux }

func (d *Driver) Name() string                                           { return "firecracker" }
func (d *Driver) Isolated() bool                                         { return true }
func (d *Driver) Jailed() bool                                           { return false }
func (d *Driver) Kernel() string                                         { return "" }
func (d *Driver) Start(context.Context, driver.Spec) (driver.Box, error) { return nil, errLinux }
func (d *Driver) BuildCacheDir() string                                  { return "" }
func (d *Driver) RootDeno() string                                       { return "" }
func (d *Driver) SetInit(string)                                         {}
func (d *Driver) EnsureBuildCache(context.Context) error                 { return errLinux }
func (d *Driver) RunBuild(context.Context, build.Invocation) ([]byte, []byte, error) {
	return nil, nil, errLinux
}
