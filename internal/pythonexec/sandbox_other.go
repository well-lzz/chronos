//go:build !darwin && !linux

package pythonexec

import (
	"context"
	"fmt"
	"os/exec"
)

func sandboxCommand(context.Context, string, interpreter, string) (*exec.Cmd, error) {
	return nil, fmt.Errorf("Python OS sandbox unsupported")
}
func configureProcess(*exec.Cmd) {}
func cleanupProcess(*exec.Cmd)   {}
