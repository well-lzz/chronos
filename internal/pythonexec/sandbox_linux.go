//go:build linux

package pythonexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

func sandboxCommand(ctx context.Context, backend string, py interpreter, dir string) (*exec.Cmd, error) {
	args := []string{"--unshare-all", "--unshare-user", "--new-session", "--die-with-parent", "--cap-drop", "ALL", "--clearenv", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--bind", dir, "/work", "--chdir", "/work", "--setenv", "HOME", "/work", "--setenv", "TMPDIR", "/tmp", "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "LANG", "C"}
	// Deliberately omit home directories, /run, host credentials and state.
	// The loader cache and interpreter's own lib directory are needed for
	// shared-library Python installations (including hosted CI toolchains).
	seen := map[string]bool{}
	for _, path := range []string{"/usr/bin", "/usr/lib", "/usr/lib64", "/usr/local/bin", "/usr/local/lib", "/lib", "/lib64", "/etc/ld.so.cache", filepath.Join(py.Prefix, "lib"), filepath.Join(py.Prefix, "lib64"), py.Stdlib, py.Executable} {
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	args = append(args, "--", py.Executable, "-I", "-S", "/work/bootstrap.py")
	return exec.CommandContext(ctx, backend, args...), nil
}
