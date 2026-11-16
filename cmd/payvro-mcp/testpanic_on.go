//go:build pv5testpanic

package main

import "os"

// maybeTestPanic panics with the active pey key when PAYVRO_TEST_PANIC=1.
// This file is excluded from the production build.
func maybeTestPanic() {
	if os.Getenv("PAYVRO_TEST_PANIC") != "1" {
		return
	}
	panic("payvro-mcp test panic: " + os.Getenv("PEY_API_KEY") + " " + os.Getenv("PAYVRO_BFF_SHARED_SECRET"))
}
