package runner

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// runnerHostsFile is the hosts file the SSRF IP-pin writes to. A var (not a
// const) so tests can point it at a temp file.
var runnerHostsFile = "/etc/hosts"

const ssrfPinMarker = "# iterion-runner ssrf-pin"

// pinHostInHostsFile appends a "<ip> <host>  # iterion-runner ssrf-pin" line to
// hostsPath so a git subprocess spawned next resolves <host> to the
// already-validated public IP — defeating a DNS-rebinding answer between
// validateRepoTarget's pre-check and git's own resolution (the SSRF TOCTOU).
//
// It returns a restore func that removes ONLY the lines this call added (it
// re-reads and filters, so a concurrent unrelated edit is preserved). The
// caller treats any error as "could not pin" and proceeds — the pod egress
// NetworkPolicy is the authoritative control. Pure on hostsPath, so it is
// unit-testable with a temp file.
func pinHostInHostsFile(hostsPath, host string, ip net.IP) (func(), error) {
	if host == "" || ip == nil {
		return nil, fmt.Errorf("ssrf-pin: empty host or ip")
	}
	orig, err := os.ReadFile(hostsPath)
	if err != nil {
		return nil, fmt.Errorf("ssrf-pin: read %s: %w", hostsPath, err)
	}
	pinLine := fmt.Sprintf("%s %s  %s", ip.String(), host, ssrfPinMarker)
	buf := orig
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}
	buf = append(buf, []byte(pinLine+"\n")...)
	if err := os.WriteFile(hostsPath, buf, 0o644); err != nil {
		return nil, fmt.Errorf("ssrf-pin: write %s: %w", hostsPath, err)
	}
	return func() { removeHostsPinLine(hostsPath, pinLine) }, nil
}

// removeHostsPinLine drops the exact pin line this run added, leaving every
// other line (incl. unrelated concurrent additions) intact. Best-effort.
func removeHostsPinLine(hostsPath, pinLine string) {
	data, err := os.ReadFile(hostsPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	kept := lines[:0]
	dropped := false
	for _, l := range lines {
		if !dropped && l == pinLine {
			dropped = true // remove only the first matching occurrence
			continue
		}
		kept = append(kept, l)
	}
	_ = os.WriteFile(hostsPath, []byte(strings.Join(kept, "\n")), 0o644)
}
