package waypoint

// CRIU compatibility checks

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var criuCheckOnce sync.Once
var criuCheckErr error

// EnsureCriuCompatible verifies the criu binary on PATH can checkpoint and
// restore processes on this host. On aarch64 hosts with pointer
// authentication (PAC), CRIU older than 4.0 does not dump/restore the
// per-process PAC keys, so any restored process built with
// -mbranch-protection (bash, glibc, most distro userland) dies with SIGILL
// at the first authenticated return after restore.
func EnsureCriuCompatible() error {
	criuCheckOnce.Do(func() {
		criuCheckErr = checkCriuCompatible()
	})
	return criuCheckErr
}

func checkCriuCompatible() error {
	major, minor, err := criuVersion()
	if err != nil {
		return fmt.Errorf("criu not usable: %w", err)
	}
	if hostHasPAC() && major < 4 {
		return fmt.Errorf(
			"criu %d.%d cannot restore processes on this host: the CPU has ARM64 "+
				"pointer authentication (paca/pacg) and CRIU only checkpoints PAC keys "+
				"since 4.0; install criu >= 4.0", major, minor)
	}
	return nil
}

var criuVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)`)

func criuVersion() (int, int, error) {
	out, err := exec.Command("criu", "--version").CombinedOutput()
	if err != nil {
		return 0, 0, fmt.Errorf("criu --version failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	m := criuVersionRe.FindSubmatch(out)
	if m == nil {
		return 0, 0, fmt.Errorf("cannot parse criu --version output: %q", strings.TrimSpace(string(out)))
	}
	major, _ := strconv.Atoi(string(m[1]))
	minor, _ := strconv.Atoi(string(m[2]))
	return major, minor, nil
}

func hostHasPAC() bool {
	if runtime.GOARCH != "arm64" {
		return false
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Features") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if f == "paca" || f == "pacg" {
				return true
			}
		}
	}
	return false
}
