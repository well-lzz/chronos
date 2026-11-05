//go:build linux

package missionproc

import (
	"fmt"
	"os"
	"os/exec"
)

func sandboxCommand(binary, root string, args, assets []string) (*exec.Cmd, error) {
	backend, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("mission sandbox unavailable: %w", err)
	}
	// Mission tools need target/model networking. The network remains governed
	// by Scope; unlike offline Python this layer intentionally shares networking.
	a := []string{"--unshare-user", "--unshare-pid", "--unshare-uts", "--unshare-ipc", "--unshare-cgroup-try", "--die-with-parent", "--cap-drop", "ALL", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--bind", root, root, "--chdir", root}
	for _, path := range []string{"/usr/bin", "/bin", "/usr/lib", "/usr/lib64", "/usr/local/bin", "/usr/local/lib", "/lib", "/lib64", "/etc/ld.so.cache", "/etc/ssl", "/etc/resolv.conf", "/etc/hosts", "/etc/services", binary} {
		if _, err := os.Stat(path); err == nil {
			a = append(a, "--ro-bind", path, path)
		}
	}
	for _, asset := range assets {
		a = append(a, "--ro-bind", asset, asset)
	}
	a = append(a, "--", binary)
	a = append(a, args...)
	return exec.Command(backend, a...), nil
}
