//go:build !windows

package main

// virtualizedState: packaged-app file system virtualization exists only on Windows.
func virtualizedState(string) string { return "" }
