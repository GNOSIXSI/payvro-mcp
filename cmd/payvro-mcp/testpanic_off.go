//go:build !pv5testpanic

package main

// maybeTestPanic is a no-op in the production build. The panic body and the
// PAYVRO_TEST_PANIC read live only in the file built with -tags pv5testpanic.
func maybeTestPanic() {}
