//go:build !darwin

package main

// createPMAssertion is a no-op on non-darwin: there's no IOKit, and on those
// platforms we already log a single "macOS only" line at startup and idle.
// Return an id of 0 so the manager logic short-circuits cleanly.
func createPMAssertion(_ string) (uint32, error) {
	return 0, errUnsupportedOS
}

func releasePMAssertion(_ uint32) error {
	return errUnsupportedOS
}
