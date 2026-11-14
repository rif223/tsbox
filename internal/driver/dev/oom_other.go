//go:build !linux

package dev

func setOOMScoreAdj(pid, adj int) error { return nil }
