package waypoint

// Private network mode: each fork runs in its own network namespace, pinned
// under NetRuntimeDir and connected outward by a pasta(1) process. Dumps mark
// the namespace external; restores put the tree into the fork's pinned one.
// Snapshot keeps a fork's namespace; park, destroy and teardown release it.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	netNSPinName = "netns"
	pastaPIDName = "pasta.pid"
	pastaLogName = "pasta.log"

	// criuNetNSKey names the external network namespace in criu images.
	criuNetNSKey = "waypoint-net"

	// dnsForwardAddr is the guest resolver in private mode; pasta forwards
	// queries sent to it to the host's resolver.
	dnsForwardAddr = "169.254.1.1"
)

// NetRuntimeDir holds each private-mode fork's namespace pin and pasta PID
// file, under <session>/<fork>. pasta opens both without CAP_DAC_OVERRIDE, so
// they stay out of the session dir, whose ancestors may be private to a user.
var NetRuntimeDir = "/run/waypoint"

func (m *Manager) sessionNetDir() string {
	return filepath.Join(NetRuntimeDir, m.sessionID)
}

// ForkNetwork is a private-mode fork's namespace pin and pasta process; the
// zero value means the host network namespace.
type ForkNetwork struct {
	NetNS          string `json:"netns,omitempty"`
	PastaPID       int    `json:"pasta_pid,omitempty"`
	PastaStartTime uint64 `json:"pasta_start_time,omitempty"`
	// NetworkLock is the session's criu --network-lock method, for the
	// restore helper, which reads only the fork record.
	NetworkLock string `json:"network_lock,omitempty"`
}

func (m *Manager) privateNetwork() bool {
	return m.network == NetworkPrivate
}

// detectNetworkLock returns "nftables" if criu supports that --network-lock
// method, else "" (criu's default, iptables).
func detectNetworkLock() string {
	if exec.Command("criu", "check", "--feature", "network_lock_nftables").Run() == nil {
		return "nftables"
	}
	return ""
}

// networkLockArgs returns the criu arguments selecting method.
func networkLockArgs(method string) []string {
	if method == "" {
		return nil
	}
	return []string{"--network-lock", method}
}

// newForkNetwork creates fork forkID's namespace and starts its pasta; the
// namespace is configured on return. pasta logs to the fork's root dir,
// which must exist.
func (m *Manager) newForkNetwork(forkID string) (ForkNetwork, error) {
	dir := filepath.Join(m.sessionNetDir(), forkID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ForkNetwork{}, err
	}
	pin := filepath.Join(dir, netNSPinName)
	releaseNetNS(pin) // stale, from an earlier fork with this ID
	if err := createNetNS(pin); err != nil {
		_ = os.Remove(dir)
		return ForkNetwork{}, err
	}
	pid, err := startPasta(pin, filepath.Join(dir, pastaPIDName), filepath.Join(m.forkDir(forkID), pastaLogName))
	if err != nil {
		releaseNetNS(pin)
		_ = os.Remove(dir)
		return ForkNetwork{}, err
	}
	startTime, _ := procStartTime(pid) // 0 on failure = unverified kill
	return ForkNetwork{NetNS: pin, PastaPID: pid, PastaStartTime: startTime, NetworkLock: m.networkLock}, nil
}

// releaseNetwork stops pasta, which holds the pin open, then removes the pin
// and the fork's runtime dir, and zeroes n.
func (n *ForkNetwork) releaseNetwork() {
	if n.PastaPID > 0 {
		if err := killTree(n.PastaPID, n.PastaStartTime); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to stop pasta: %v\n", err)
		}
	}
	if n.NetNS != "" {
		releaseNetNS(n.NetNS)
		dir := filepath.Dir(n.NetNS)
		_ = os.Remove(filepath.Join(dir, pastaPIDName))
		_ = os.Remove(dir)
	}
	*n = ForkNetwork{}
}

// sweepSessionNetwork kills the session's remaining pasta processes and
// removes its runtime dir, including plumbing no fork record points to.
func (m *Manager) sweepSessionNetwork() {
	if m.sessionID == "" {
		return
	}
	dir := m.sessionNetDir()
	killSessionPasta(dir)
	unmountAll(sessionMounts(dir))
	_ = os.RemoveAll(dir)
}

// withThreadNetNS runs fn on a locked OS thread that enter has moved to
// another network namespace, then moves the thread back. If that fails, the
// thread stays locked and exits with the goroutine.
func withThreadNetNS(enter, fn func() error) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			errCh <- err
			return
		}
		defer orig.Close()
		if err := enter(); err != nil {
			runtime.UnlockOSThread()
			errCh <- err
			return
		}
		fnErr := fn()
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("return to the original network namespace: %w", err)
			return
		}
		runtime.UnlockOSThread()
		errCh <- fnErr
	}()
	return <-errCh
}

// createNetNS creates a network namespace with loopback up and pins it with
// a bind mount at pin.
func createNetNS(pin string) error {
	file, err := os.OpenFile(pin, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return fmt.Errorf("create network namespace pin: %w", err)
	}
	file.Close()

	err = withThreadNetNS(
		func() error {
			if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
				return fmt.Errorf("unshare network namespace: %w", err)
			}
			return nil
		},
		func() error {
			if err := unix.Mount("/proc/thread-self/ns/net", pin, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("pin network namespace at %s: %w", pin, err)
			}
			return bringLoopbackUp()
		},
	)
	if err != nil {
		releaseNetNS(pin)
	}
	return err
}

// releaseNetNS unmounts and removes a namespace pin.
func releaseNetNS(pin string) {
	if err := unix.Unmount(pin, 0); err != nil && err != unix.EINVAL && err != unix.ENOENT {
		_ = unix.Unmount(pin, unix.MNT_DETACH)
	}
	_ = os.Remove(pin)
}

// startInNetNS starts cmd in the network namespace pinned at pin.
func startInNetNS(pin string, cmd *exec.Cmd) error {
	ns, err := os.Open(pin)
	if err != nil {
		return fmt.Errorf("open network namespace %s: %w", pin, err)
	}
	defer ns.Close()
	return withThreadNetNS(
		func() error {
			if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWNET); err != nil {
				return fmt.Errorf("enter network namespace %s: %w", pin, err)
			}
			return nil
		},
		cmd.Start,
	)
}

// bringLoopbackUp raises lo in the calling thread's network namespace.
func bringLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// pastaArgs is the pasta command line for the namespace pinned at pin. No
// ports are forwarded. The default gateway mapping stays on: the gateway
// address reaches host loopback services, including a loopback resolver.
func pastaArgs(pidFile, pin string) []string {
	return []string{
		// As root, pasta otherwise drops to nobody, which cannot enter the pin.
		"--runas", "0",
		"--quiet",
		"--config-net",
		"--tcp-ports", "none",
		"--udp-ports", "none",
		"--tcp-ns", "none",
		"--udp-ns", "none",
		"--dns-forward", dnsForwardAddr,
		"--pid", pidFile,
		"--netns", pin,
	}
}

// startPasta starts pasta for the namespace pinned at pin and returns the
// daemon's PID from pidFile; the namespace is configured once the foreground
// process exits. Output goes to a file: the daemon could hold a pipe open.
func startPasta(pin, pidFile, logPath string) (int, error) {
	bin, err := exec.LookPath("pasta")
	if err != nil {
		return 0, fmt.Errorf("private network mode needs pasta (package passt): %w", err)
	}
	_ = os.Remove(pidFile)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	cmd := exec.Command(bin, pastaArgs(pidFile, pin)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("pasta failed: %w\n%s", err, readRecentFile(logPath, 4096))
	}
	pid, err := readPIDFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("pasta did not record its PID: %w", err)
	}
	return pid, nil
}

// externalNetNSArg returns the criu dump --external value marking the network
// namespace of pid as external, or "" if it is this thread's (the host's).
func externalNetNSArg(pid int) (string, error) {
	var tree, self unix.Stat_t
	if err := unix.Stat(fmt.Sprintf("/proc/%d/ns/net", pid), &tree); err != nil {
		return "", fmt.Errorf("stat network namespace of pid %d: %w", pid, err)
	}
	if err := unix.Stat("/proc/thread-self/ns/net", &self); err != nil {
		return "", err
	}
	if tree.Dev == self.Dev && tree.Ino == self.Ino {
		return "", nil
	}
	return fmt.Sprintf("net[%d]:%s", tree.Ino, criuNetNSKey), nil
}

// privateResolvConf is the private-mode guest resolv.conf: pasta's forwarder
// as the only nameserver, plus the host's search domains and options.
func privateResolvConf(host []byte) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "nameserver %s\n", dnsForwardAddr)
	for _, line := range strings.Split(string(host), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "search", "domain", "options":
			b.WriteString(strings.Join(fields, " ") + "\n")
		}
	}
	return []byte(b.String())
}

// killSessionPasta kills pasta processes whose --netns is under dir,
// re-checked after a pidfd pins each process.
func killSessionPasta(dir string) {
	base := filepath.Clean(dir)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !isSessionPasta(pid, base) {
			continue
		}
		pfd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			continue
		}
		if isSessionPasta(pid, base) {
			_ = unix.PidfdSendSignal(pfd, unix.SIGKILL, nil, 0)
		}
		unix.Close(pfd)
	}
}

func isSessionPasta(pid int, base string) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	return pastaServes(strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), base)
}

// pastaServes reports whether argv is a pasta command line whose --netns
// argument is under base.
func pastaServes(argv []string, base string) bool {
	if len(argv) == 0 || !strings.HasPrefix(filepath.Base(argv[0]), "pasta") {
		return false
	}
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "--netns" && pathUnder(argv[i+1], base) {
			return true
		}
	}
	return false
}
