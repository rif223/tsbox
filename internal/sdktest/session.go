// Package sdktest provides a session for provider tests.
package sdktest

import (
	"log/slog"
	"sync"

	"github.com/rif223/tsbox/sdk"
)

// Session is a test session with fixed grants and config; events and logs are collected.
type Session struct {
	BoxRef    sdk.BoxRef
	ScriptRef sdk.ScriptRef
	G         sdk.Grants
	C         sdk.Config

	mu     sync.Mutex
	Events []string
	Logs   []string
}

func (s *Session) Box() sdk.BoxRef       { return s.BoxRef }
func (s *Session) Script() sdk.ScriptRef { return s.ScriptRef }
func (s *Session) Grants() sdk.Grants    { return s.G }
func (s *Session) Config() sdk.Config    { return s.C }
func (s *Session) Logger() *slog.Logger  { return slog.Default() }

func (s *Session) Emit(event string, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, event)
	return nil
}

func (s *Session) ScriptLog(level, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Logs = append(s.Logs, level+" "+msg)
}
