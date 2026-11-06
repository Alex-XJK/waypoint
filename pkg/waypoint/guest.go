package waypoint

// Guest options: per-session customization of what the guest sees, applied
// when the session's shell starts (init --shell or build). Like the rest of
// the guest environment they are process and filesystem state, so every
// checkpoint and fork of the session inherits them.

import (
	"fmt"
	"os"
	"strings"
)

// GuestOptions customize a new session's guest.
type GuestOptions struct {
	// Env entries (KEY=VALUE) override the image's ENV.
	Env []string
	// WorkDir replaces the image's WORKDIR as the shell's starting directory.
	WorkDir string
	// Hostname replaces the default "waypoint".
	Hostname string
	// Hosts are /etc/hosts lines ("<address> <name>...") appended to the
	// rootfs's file, which stays the session's own copy.
	Hosts []string
}

// SetGuestOptions makes StartShell apply o. Call it before init's StartShell
// or BuildEnvironment.
func (m *Manager) SetGuestOptions(o GuestOptions) error {
	for _, e := range o.Env {
		if envKey(e) == "" {
			return fmt.Errorf("invalid guest environment entry %q: want KEY=VALUE", e)
		}
		if strings.HasPrefix(e, "WAYPOINT_") {
			return fmt.Errorf("invalid guest environment entry %q: WAYPOINT_* is reserved", e)
		}
	}
	if o.WorkDir != "" && !strings.HasPrefix(o.WorkDir, "/") {
		return fmt.Errorf("guest working directory %q must be absolute", o.WorkDir)
	}
	if o.Hostname != "" && !validHostname(o.Hostname) {
		return fmt.Errorf("invalid guest hostname %q", o.Hostname)
	}
	for _, line := range o.Hosts {
		if strings.ContainsAny(line, "\n\r") || len(strings.Fields(line)) < 2 {
			return fmt.Errorf("invalid hosts line %q: want \"<address> <name>...\"", line)
		}
	}
	m.guest = o
	return nil
}

// ImageCommand returns the built image's default ENTRYPOINT and CMD; both are
// empty for an `init` session.
func (m *Manager) ImageCommand() (entrypoint, cmd []string) {
	return m.imageEntrypoint, m.imageCmd
}

// validHostname accepts host names of up to 64 bytes (the UTS limit) made of
// dot-separated labels of letters, digits, '-', and '_' (which compose service
// names may contain, and which the kernel and /etc/hosts lookups accept).
func validHostname(name string) bool {
	if len(name) > 64 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// appendGuestHosts appends lines to /etc/hosts of the merged rootfs at
// rootfs. The file is opened through an os.Root, so an image whose /etc/hosts
// is a symlink cannot redirect the write outside the rootfs.
func appendGuestHosts(rootfs string, lines []string) error {
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll("etc", 0o755); err != nil {
		return fmt.Errorf("prepare /etc/hosts: %w", err)
	}
	f, err := root.OpenFile("etc/hosts", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open /etc/hosts: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		return fmt.Errorf("write /etc/hosts: %w", err)
	}
	return nil
}
