package core

import (
	"errors"
	"os"
)

// Core dumps are only handled on Linux nodes; the Windows CLI never calls these.
var errUnsupported = errors.New("core handling is only supported on Linux")

func diskSpace(string) (uint64, uint64, error) { return 0, 0, errUnsupported }
func lockFile(*os.File) error                  { return errUnsupported }

var statDisk = diskSpace
