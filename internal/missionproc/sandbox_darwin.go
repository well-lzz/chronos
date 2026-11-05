//go:build darwin

package missionproc

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func sandboxCommand(binary, root string, args, assets []string) (*exec.Cmd, error) {
	backend, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, fmt.Errorf("mission sandbox unavailable: %w", err)
	}
	var p strings.Builder
	p.WriteString(`(version 1)(deny default)(allow process-exec process-fork)(allow signal (target self) (target children))(allow sysctl-read)(allow file-read-metadata file-test-existence)(allow file-read* (literal "/"))(allow network-outbound)(allow network-bind network-inbound (subpath ` + strconv.Quote(root) + `))`)
	p.WriteString(`(allow mach-lookup (global-name "com.apple.trustd") (global-name "com.apple.trustd.agent") (global-name "com.apple.system.opendirectoryd.libinfo"))`)
	p.WriteString(`(allow network-bind network-inbound (local ip "localhost:*"))`)
	for _, asset := range assets {
		p.WriteString("(allow file-read* (subpath " + strconv.Quote(asset) + "))")
	}
	for _, path := range []string{binary, root, "/System/Library", "/usr/lib", "/private/var/db/dyld", "/usr/bin", "/bin", "/usr/local/bin", "/usr/local/lib", "/opt/homebrew/bin", "/opt/homebrew/lib", "/Library/Frameworks/Python.framework", "/etc/ssl", "/private/etc/ssl", "/private/etc/resolv.conf", "/private/var/run/resolv.conf", "/private/etc/hosts", "/private/etc/services", "/private/etc/protocols", "/dev/null", "/dev/urandom", "/dev/random"} {
		p.WriteString("(allow file-read* (subpath " + strconv.Quote(path) + "))")
	}
	p.WriteString("(allow file-write* (subpath " + strconv.Quote(root) + "))(allow file-write-data (literal \"/dev/null\"))")
	a := []string{"-p", p.String(), binary}
	a = append(a, args...)
	return exec.Command(backend, a...), nil
}
