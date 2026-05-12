//go:build !darwin

package main

import (
	"context"
	"errors"
	"time"
)

// errUnsupportedOS is returned by every pmset shim on non-macOS platforms.
// The daemon catches it once at startup and degrades to a logging no-op
// instead of crash-looping under launchd / systemd.
var errUnsupportedOS = errors.New("bigband-wake: pmset wake scheduling is macOS-only")

func schedulePmsetWake(_ context.Context, _ time.Time) error { return errUnsupportedOS }
func cancelPmsetWake(_ context.Context, _ time.Time) error   { return errUnsupportedOS }
func pmsetReachable(_ context.Context) error                 { return errUnsupportedOS }
func dumpPmsetSched(_ context.Context) (string, error)       { return "", errUnsupportedOS }
