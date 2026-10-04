package waypoint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrivateResolvConfUsesOnlyTheForwarder(t *testing.T) {
	host := "# generated\nnameserver 127.0.0.53\nnameserver 10.0.0.2\noptions edns0  trust-ad\nsearch  a.example b.example\n"
	want := "nameserver " + dnsForwardAddr + "\noptions edns0 trust-ad\nsearch a.example b.example\n"
	if got := string(privateResolvConf([]byte(host))); got != want {
		t.Fatalf("privateResolvConf() = %q, want %q", got, want)
	}
	if got := string(privateResolvConf(nil)); got != "nameserver "+dnsForwardAddr+"\n" {
		t.Fatalf("privateResolvConf(nil) = %q", got)
	}
}

func TestPrepareNetworkDepsPrivateSeedsTheForwarder(t *testing.T) {
	rootfs := t.TempDir()
	if err := PrepareNetworkDeps(rootfs, NetworkPrivate); err != nil {
		t.Fatalf("PrepareNetworkDeps(): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rootfs, "etc", "resolv.conf"))
	if err != nil {
		t.Fatalf("ReadFile(resolv.conf): %v", err)
	}
	if !strings.HasPrefix(string(data), "nameserver "+dnsForwardAddr+"\n") || strings.Count(string(data), "nameserver") != 1 {
		t.Fatalf("resolv.conf = %q, want the forwarder as the only nameserver", data)
	}
}

func TestPrepareNetworkDepsKeepsTheImageResolvConf(t *testing.T) {
	for _, mode := range []string{NetworkShared, NetworkPrivate} {
		t.Run(mode, func(t *testing.T) {
			rootfs := t.TempDir()
			path := filepath.Join(rootfs, "etc", "resolv.conf")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			const image = "nameserver 192.0.2.53\n"
			if err := os.WriteFile(path, []byte(image), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := PrepareNetworkDeps(rootfs, mode); err != nil {
				t.Fatalf("PrepareNetworkDeps(): %v", err)
			}
			if data, _ := os.ReadFile(path); string(data) != image {
				t.Fatalf("resolv.conf = %q, want the image's %q", data, image)
			}
		})
	}
}

// A blank name-resolution file that is a symlink must be replaced, not
// written through: an absolute target resolves against the host root.
func TestPrepareNetworkDepsDoesNotWriteThroughSymlinks(t *testing.T) {
	for _, mode := range []string{NetworkShared, NetworkPrivate} {
		t.Run(mode, func(t *testing.T) {
			rootfs := t.TempDir()
			outside := t.TempDir()
			if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"resolv.conf", "hosts", "nsswitch.conf"} {
				if err := os.Symlink(filepath.Join(outside, name), filepath.Join(rootfs, "etc", name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := PrepareNetworkDeps(rootfs, mode); err != nil {
				t.Fatalf("PrepareNetworkDeps(): %v", err)
			}
			for _, name := range []string{"resolv.conf", "hosts", "nsswitch.conf"} {
				if _, err := os.Lstat(filepath.Join(outside, name)); !os.IsNotExist(err) {
					t.Errorf("%s was written through the symlink (Lstat error = %v)", name, err)
				}
				fi, err := os.Lstat(filepath.Join(rootfs, "etc", name))
				if name == "resolv.conf" && mode == NetworkShared {
					if _, hostErr := os.Stat("/etc/resolv.conf"); hostErr != nil {
						continue // no host file to copy: the dangling link stays
					}
				}
				if err != nil || !fi.Mode().IsRegular() {
					t.Errorf("%s in the rootfs is not a regular file (mode %v, error %v)", name, fi.Mode(), err)
				}
			}
		})
	}
}

func TestPastaArgsForwardNoPorts(t *testing.T) {
	args := pastaArgs("/run/x/pasta.pid", "/run/x/netns")
	value := func(flag string) string {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			return ""
		}
		return args[i+1]
	}
	for _, flag := range []string{"--tcp-ports", "--udp-ports", "--tcp-ns", "--udp-ns"} {
		if got := value(flag); got != "none" {
			t.Errorf("%s = %q, want none", flag, got)
		}
	}
	for flag, want := range map[string]string{
		"--netns":       "/run/x/netns",
		"--pid":         "/run/x/pasta.pid",
		"--runas":       "0",
		"--dns-forward": dnsForwardAddr,
	} {
		if got := value(flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if !slices.Contains(args, "--config-net") {
		t.Errorf("args %q lack --config-net", args)
	}
}

func TestPastaServesMatchesOnlyNamespacesUnderTheDirectory(t *testing.T) {
	base := "/run/waypoint/0123456789abcdef"
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"pasta", "--quiet", "--netns", base + "/main/netns"}, true},
		{[]string{"/usr/bin/pasta", "--netns", base + "/f1/netns", "--pid", base + "/f1/pasta.pid"}, true},
		{[]string{"pasta.avx2", "--netns", base + "/f1/netns"}, true},
		{[]string{"pasta", "--netns", "/run/waypoint/fedcba9876543210/main/netns"}, false},
		{[]string{"pasta", "--netns", base + "0/main/netns"}, false},
		{[]string{"sleep", "--netns", base + "/main/netns"}, false},
		{[]string{"bash", "-c", "pasta --netns " + base + "/main/netns"}, false},
		{[]string{"pasta", "--pid", base + "/main/pasta.pid"}, false},
		{[]string{"pasta", "--netns"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := pastaServes(tc.argv, base); got != tc.want {
			t.Errorf("pastaServes(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestExternalNetNSArgIsEmptyInTheHostNamespace(t *testing.T) {
	arg, err := externalNetNSArg(os.Getpid())
	if err != nil {
		t.Fatalf("externalNetNSArg(self): %v", err)
	}
	if arg != "" {
		t.Fatalf("externalNetNSArg(self) = %q, want empty", arg)
	}
}

func TestSessionRecordsItsNetworkMode(t *testing.T) {
	// Short paths: the sessions dir is bounded by the socket path limit.
	dir, err := os.MkdirTemp("", "wpnet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	previous := []string{SessionInfoDir, SessionInfoDirConfig, DefaultSessionsDir, DefaultSessionsDirConfig, NetworkMode, NetworkModeConfig}
	t.Cleanup(func() {
		SessionInfoDir, SessionInfoDirConfig = previous[0], previous[1]
		DefaultSessionsDir, DefaultSessionsDirConfig = previous[2], previous[3]
		NetworkMode, NetworkModeConfig = previous[4], previous[5]
	})
	cfg := fmt.Sprintf(`{"sessions_dir": %q, "session_info_dir": %q, "network": "private"}`,
		filepath.Join(dir, "s"), filepath.Join(dir, "i"))
	loadBooleanConfigForTest(t, cfg, nil)

	_, sessionID, err := NewManagerWithSession()
	if err != nil {
		t.Fatalf("NewManagerWithSession(): %v", err)
	}
	info, err := LoadSessionInfo(sessionID)
	if err != nil {
		t.Fatalf("LoadSessionInfo(): %v", err)
	}
	if info.Network != NetworkPrivate {
		t.Fatalf("SessionInfo.Network = %q, want %q", info.Network, NetworkPrivate)
	}

	// The recorded mode wins over the current configuration.
	t.Setenv("WAYPOINT_NETWORK", NetworkShared)
	m, err := LoadManager(sessionID)
	if err != nil {
		t.Fatalf("LoadManager(): %v", err)
	}
	if !m.privateNetwork() {
		t.Fatalf("loaded session is not in private network mode")
	}

	// A record from before network modes existed loads as shared.
	info.Network = ""
	if err := writeSessionInfo(info); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_NETWORK", NetworkPrivate)
	if m, err = LoadManager(sessionID); err != nil {
		t.Fatalf("LoadManager(): %v", err)
	}
	if m.privateNetwork() {
		t.Fatalf("a record without a network mode loaded as private")
	}
}

// The tests below need root (CAP_SYS_ADMIN for unshare/setns and mounts).

func TestCreateNetNSAndStartInside(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	var host unix.Stat_t
	if err := unix.Stat("/proc/self/ns/net", &host); err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(t.TempDir(), "netns")
	if err := createNetNS(pin); err != nil {
		t.Fatalf("createNetNS(): %v", err)
	}
	t.Cleanup(func() { releaseNetNS(pin) })
	var ns unix.Stat_t
	if err := unix.Stat(pin, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Ino == host.Ino {
		t.Fatalf("the pin is the host network namespace")
	}

	cmd := exec.Command("sleep", "30")
	if err := startInNetNS(pin, cmd); err != nil {
		t.Fatalf("startInNetNS(): %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var child unix.Stat_t
	if err := unix.Stat(fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid), &child); err != nil {
		t.Fatal(err)
	}
	if child.Ino != ns.Ino {
		t.Fatalf("child runs in network namespace %d, want %d", child.Ino, ns.Ino)
	}
	want := fmt.Sprintf("net[%d]:%s", ns.Ino, criuNetNSKey)
	if arg, err := externalNetNSArg(cmd.Process.Pid); err != nil || arg != want {
		t.Fatalf("externalNetNSArg(child) = %q, %v; want %q", arg, err, want)
	}

	if out, err := exec.Command("nsenter", "--net="+pin, "ip", "-o", "link", "show", "lo").CombinedOutput(); err == nil {
		if !strings.Contains(string(out), ",UP") {
			t.Fatalf("loopback is not up in the new namespace: %s", out)
		}
	}

	// Neither call may leave this process in another namespace.
	var after unix.Stat_t
	if err := unix.Stat("/proc/self/ns/net", &after); err != nil {
		t.Fatal(err)
	}
	if after.Ino != host.Ino {
		t.Fatalf("the process moved to network namespace %d", after.Ino)
	}
}

func TestReleaseNetNSRemovesThePin(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	pin := filepath.Join(t.TempDir(), "netns")
	if err := createNetNS(pin); err != nil {
		t.Fatalf("createNetNS(): %v", err)
	}
	releaseNetNS(pin)
	if _, err := os.Lstat(pin); !os.IsNotExist(err) {
		t.Fatalf("pin still present after release: %v", err)
	}
	if mounts := sessionMounts(filepath.Dir(pin)); len(mounts) != 0 {
		t.Fatalf("mounts left under %s: %v", filepath.Dir(pin), mounts)
	}
}

func TestForkNetworkLifecycle(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("pasta"); err != nil {
		t.Skip("pasta not installed")
	}
	previous := NetRuntimeDir
	NetRuntimeDir = t.TempDir()
	t.Cleanup(func() { NetRuntimeDir = previous })
	m := newManager(t.TempDir())
	m.sessionID = "0123456789abcdef"
	if err := os.MkdirAll(m.forkDir("f1"), 0o755); err != nil {
		t.Fatal(err)
	}

	n, err := m.newForkNetwork("f1")
	if err != nil {
		t.Fatalf("newForkNetwork(): %v", err)
	}
	if n.NetNS != filepath.Join(NetRuntimeDir, m.sessionID, "f1", netNSPinName) || n.PastaPID <= 0 {
		t.Fatalf("newForkNetwork() = %+v", n)
	}
	if !isSessionPasta(n.PastaPID, m.sessionNetDir()) {
		t.Fatalf("pasta %d is not recognized as serving the session", n.PastaPID)
	}
	if out, err := exec.Command("nsenter", "--net="+n.NetNS, "ip", "-o", "-4", "route", "show", "default").CombinedOutput(); err == nil && len(strings.TrimSpace(string(out))) == 0 {
		t.Fatalf("no default route in the fork namespace")
	}

	pid := n.PastaPID
	n.releaseNetwork()
	if processExists(pid) {
		t.Fatalf("pasta %d still running after release", pid)
	}
	if n != (ForkNetwork{}) {
		t.Fatalf("record not cleared: %+v", n)
	}
	if _, err := os.Stat(filepath.Join(m.sessionNetDir(), "f1")); !os.IsNotExist(err) {
		t.Fatalf("fork runtime directory left behind: %v", err)
	}

	// A leaked pasta (no fork record) is found by the session sweep.
	leaked, err := m.newForkNetwork("f1")
	if err != nil {
		t.Fatalf("newForkNetwork(): %v", err)
	}
	m.sweepSessionNetwork()
	if err := awaitExit(leaked.PastaPID); err != nil {
		t.Fatalf("sweep left pasta running: %v", err)
	}
	if _, err := os.Stat(m.sessionNetDir()); !os.IsNotExist(err) {
		t.Fatalf("session runtime directory left behind: %v", err)
	}
}

func TestSweepSessionNetworkNeedsASessionID(t *testing.T) {
	previous := NetRuntimeDir
	NetRuntimeDir = t.TempDir()
	t.Cleanup(func() { NetRuntimeDir = previous })
	other := filepath.Join(NetRuntimeDir, "0123456789abcdef")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	(&Manager{}).sweepSessionNetwork()
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("sweep without a session ID removed another session's runtime dir: %v", err)
	}
}
