//go:build !linux && !windows && !darwin

package main

import "errors"

func detectService() service { return service{} }

func installService(string) error { return errors.New("no service manager supported on this system") }

func removeService() error { return nil }

const serviceManagesProcess = false
