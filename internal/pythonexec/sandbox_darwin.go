//go:build darwin

package pythonexec

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func sandboxCommand(ctx context.Context, backend string, py interpreter, dir string) (*exec.Cmd, error) {
	// The kernel denies network access (including Unix sockets), subprocess
	// creation, user-home reads and writes outside this invocation's directory.
	var profile strings.Builder
	profile.WriteString("(version 1)(deny default)(allow process-exec)(allow sysctl-read)(allow file-read-metadata)(allow file-test-existence)(allow file-read* (literal \"/\"))")
	read := []string{py.Executable, py.Stdlib, "/System/Library", "/usr/lib", "/private/var/db/dyld", dir}
	if py.Prefix != "/" && py.Prefix != "/usr" && py.Prefix != "/usr/local" {
		read = append(read, py.Prefix)
	}
	for _, path := range read {
		profile.WriteString("(allow file-read* (subpath " + strconv.Quote(path) + "))")
	}
	profile.WriteString("(allow file-write* (subpath " + strconv.Quote(dir) + "))(allow file-write-data (literal \"/dev/null\"))")
	return exec.CommandContext(ctx, backend, "-p", profile.String(), py.Executable, "-I", "-S", filepath.Join(dir, "bootstrap.py")), nil
}
