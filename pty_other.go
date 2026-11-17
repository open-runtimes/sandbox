//go:build !linux

package main

import (
	"errors"
	"os"
)

// Sandboxes run on Linux. Elsewhere the server still builds for local use,
// without terminals.
var errNoPTY = errors.New("terminals are only supported on linux")

func openPTY() (*os.File, *os.File, error) { return nil, nil, errNoPTY }

func resizePTY(*os.File, int, int) error { return errNoPTY }
