//go:build !darwin && !linux

package durable

import (
	"fmt"
	"os"
)

func LockDir(dir string) (*os.File, error) {
	return nil, fmt.Errorf("durable state directory locking is unsupported on this operating system")
}
