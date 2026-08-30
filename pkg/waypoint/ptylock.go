package waypoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// devpts is host-global and its kernel API has no way to ask for a specific
// index: to reclaim one, CRIU opens /dev/ptmx over and over, holding every
// wrong index it is handed, until the kernel returns the one it wants
// (criu/tty.c). Two of those searches running at once cross paths — one
// transiently holds the index the other is waiting for, and the restore fails:
//
//	tty: Unable to open dev/ptmx with specified index 9
//	Error (criu/cr-restore.c): Restoring FAILED.
//
// A session's process is dead from `criu dump` until `criu restore`, so its
// index sits free in a pool that hands out the LOWEST free number first —
// which is exactly the number the restore is about to ask for. Serial runs
// never notice: nothing else allocates in that window.
//
// This lock closes the window. It is cross-PROCESS by necessity — each trial
// runs its own waypoint binary, so an in-process mutex would coordinate
// nothing. flock is used rather than a pidfile because the kernel releases it
// when the holder dies; waypoint gets killed often enough that a stale lock
// would otherwise wedge every session on the host.
//
// It does NOT isolate: sessions still share one devpts, so an unrelated
// program (an ssh login, tmux) can still steal an index. That needs a private
// devpts instance per session, at which point this lock can be deleted.
const ptyLockPath = "/run/waypoint/pty.lock"

// ptyLockWait bounds the wait. A wedged holder must not freeze every other
// session forever; on timeout we proceed and log, because the worst case is
// the race we had before the lock, which beats a deadlocked host.
const ptyLockWait = 120 * time.Second

// WithPTYLock serializes a critical section that frees and then reclaims a
// devpts index.
func WithPTYLock(what string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(ptyLockPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: pty lock dir: %v; proceeding unlocked\n", err)
		return fn()
	}
	f, err := os.OpenFile(ptyLockPath, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: pty lock open: %v; proceeding unlocked\n", err)
		return fn()
	}
	defer f.Close()

	deadline := time.Now().Add(ptyLockWait)
	locked := false
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			locked = true
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			fmt.Fprintf(os.Stderr, "warning: pty lock (%s): %v; proceeding unlocked\n", what, err)
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "warning: pty lock (%s) timed out after %s; proceeding unlocked\n", what, ptyLockWait)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if locked {
		defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	}
	return fn()
}
