package waypoint

// Host/fork file transfer. Guest paths are resolved through an os.Root opened
// on /proc/<pid>/root, so ".." and ordinary symlink escapes fail closed. This
// is intended for regular rootfs paths, not as a security boundary around
// mounted synthetic filesystems such as /proc.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// CopyToFork copies hostSource to the exact guestDestination path in a running
// fork. Parent directories are created as needed. Existing directories are
// merged; regular files are replaced atomically.
//
// Regular files and directories are supported. Source symlinks, devices,
// sockets, and named pipes are rejected. Permission bits are preserved for
// files and newly created directories; merged directories retain their mode.
// Ownership, timestamps, sparse layout, and hard-link relationships are not
// preserved. A merge is incremental, so an error can leave earlier entries.
func (m *Manager) CopyToFork(forkID, hostSource, guestDestination string) error {
	if err := validateForkID(forkID); err != nil {
		return err
	}
	if hostSource == "" {
		return errors.New("host source must not be empty")
	}
	// Clean removes trailing separators and "/." so Lstat below inspects the
	// source object itself instead of allowing the kernel to follow a symlink.
	hostSource = filepath.Clean(hostSource)
	guestDestination, err := cleanRootPath(guestDestination)
	if err != nil {
		return fmt.Errorf("invalid guest destination: %w", err)
	}
	if guestDestination == "." {
		return fmt.Errorf("invalid guest destination: the fork root cannot be replaced")
	}

	return m.withForkLock(forkID, func() error {
		root, err := m.openRunningForkRoot(forkID)
		if err != nil {
			return err
		}
		defer root.Close()

		return copyIntoRoot(hostCopySource{}, hostSource, root, guestDestination)
	})
}

// CopyFromFork copies guestSource from a running fork to the exact
// hostDestination path. Parent directories are created as needed. Existing
// directories are merged; regular files are replaced atomically.
//
// The supported file types and preserved metadata are the same as CopyToFork.
func (m *Manager) CopyFromFork(forkID, guestSource, hostDestination string) error {
	if err := validateForkID(forkID); err != nil {
		return err
	}
	guestSource, err := cleanRootPath(guestSource)
	if err != nil {
		return fmt.Errorf("invalid guest source: %w", err)
	}
	hostDestination, err = cleanHostDestination(hostDestination)
	if err != nil {
		return err
	}

	return m.withForkLock(forkID, func() error {
		forkRoot, err := m.openRunningForkRoot(forkID)
		if err != nil {
			return err
		}
		defer forkRoot.Close()

		parent := filepath.Dir(hostDestination)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("create host destination parent: %w", err)
		}
		hostRoot, err := os.OpenRoot(parent)
		if err != nil {
			return fmt.Errorf("open host destination parent: %w", err)
		}
		defer hostRoot.Close()

		return copyIntoRoot(rootCopySource{root: forkRoot}, guestSource, hostRoot, filepath.Base(hostDestination))
	})
}

// openRunningForkRoot verifies the persisted process identity on both sides of
// opening /proc/<pid>/root. The opened root then pins the filesystem view used
// by the copy even if the process exits after the second verification.
// The caller must hold the fork lock.
func (m *Manager) openRunningForkRoot(forkID string) (*os.Root, error) {
	f, err := m.loadFork(forkID)
	if err != nil {
		return nil, err
	}
	if f.Status != ForkStatusRunning {
		return nil, fmt.Errorf("fork %s is not running (status=%s)", forkID, f.Status)
	}
	if f.PID <= 0 || f.StartTime == 0 {
		return nil, fmt.Errorf("fork %s has no verifiable process identity", forkID)
	}
	if err := verifyForkProcessIdentity(forkID, f.PID, f.StartTime); err != nil {
		return nil, err
	}

	procRoot := filepath.Join("/proc", strconv.Itoa(f.PID), "root")
	root, err := os.OpenRoot(procRoot)
	if err != nil {
		return nil, fmt.Errorf("open root for fork %s: %w", forkID, err)
	}
	if err := verifyForkProcessIdentity(forkID, f.PID, f.StartTime); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func verifyForkProcessIdentity(forkID string, pid int, wantStartTime uint64) error {
	startTime, err := procStartTime(pid)
	if err != nil {
		return fmt.Errorf("cannot verify process identity for fork %s (pid=%d): %w", forkID, pid, err)
	}
	if startTime != wantStartTime {
		return fmt.Errorf("process identity for fork %s changed (pid=%d, start_time=%d, want %d)", forkID, pid, startTime, wantStartTime)
	}
	return nil
}

// cleanRootPath converts an absolute-or-relative guest spelling to the
// local, relative spelling required by os.Root. Clean also removes a trailing
// slash before any Root operation; this is important for older Go 1.25
// releases whose Root handling of a final symlink followed by "/" was unsafe.
func cleanRootPath(name string) (string, error) {
	if name == "" {
		return "", errors.New("path must not be empty")
	}
	name = strings.TrimLeft(name, string(filepath.Separator))
	if name == "" {
		return ".", nil
	}
	name = filepath.Clean(name)
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("path %q is outside the root", name)
	}
	return name, nil
}

func cleanHostDestination(name string) (string, error) {
	if name == "" {
		return "", errors.New("host destination must not be empty")
	}
	name = filepath.Clean(name)
	if name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("host destination %q cannot be replaced", name)
	}
	return name, nil
}

type copySource interface {
	lstat(string) (os.FileInfo, error)
	open(string) (*os.File, error)
}

type hostCopySource struct{}

func (hostCopySource) lstat(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}

func (hostCopySource) open(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

type rootCopySource struct {
	root *os.Root
}

func (s rootCopySource) lstat(name string) (os.FileInfo, error) {
	return s.root.Lstat(name)
}

func (s rootCopySource) open(name string) (*os.File, error) {
	return s.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}

func copyIntoRoot(source copySource, sourceName string, destination *os.Root, destinationName string) error {
	sourceFile, sourceInfo, err := openCopySource(source, sourceName)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	if sourceInfo.Mode().IsRegular() {
		return copyRegularFile(sourceFile, sourceInfo, destination, destinationName)
	}
	return copyDirectory(sourceName, sourceFile, sourceInfo, destination, destinationName)
}

func openCopySource(source copySource, name string) (*os.File, os.FileInfo, error) {
	info, err := source.lstat(name)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect source %s: %w", name, err)
	}
	if err := validateCopySourceType(name, info.Mode()); err != nil {
		return nil, nil, err
	}

	f, err := source.open(name)
	if err != nil {
		return nil, nil, fmt.Errorf("open source %s: %w", name, err)
	}
	openedInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("inspect opened source %s: %w", name, err)
	}
	if err := validateCopySourceType(name, openedInfo.Mode()); err != nil {
		f.Close()
		return nil, nil, err
	}
	if info.IsDir() != openedInfo.IsDir() {
		f.Close()
		return nil, nil, fmt.Errorf("source %s changed type while opening", name)
	}
	return f, openedInfo, nil
}

func validateCopySourceType(name string, mode os.FileMode) error {
	if mode&os.ModeSymlink != 0 {
		return fmt.Errorf("cannot copy source %s: symbolic links are not supported", name)
	}
	if mode.IsRegular() || mode.IsDir() {
		return nil
	}
	return fmt.Errorf("cannot copy source %s: special file type %s is not supported", name, mode.Type())
}

func copyRegularFile(source *os.File, sourceInfo os.FileInfo, destination *os.Root, destinationName string) error {
	parentName := filepath.Dir(destinationName)
	if err := destination.MkdirAll(parentName, 0o755); err != nil {
		return fmt.Errorf("create destination parent %s: %w", parentName, err)
	}
	parent, err := destination.OpenRoot(parentName)
	if err != nil {
		return fmt.Errorf("open destination parent %s: %w", parentName, err)
	}
	defer parent.Close()

	base := filepath.Base(destinationName)
	if info, err := parent.Lstat(base); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("destination %s exists and is not a regular file", destinationName)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect destination %s: %w", destinationName, err)
	}

	tempName, err := newCopyTempName()
	if err != nil {
		return err
	}
	out, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary destination: %w", err)
	}
	tempExists := true
	defer func() {
		if tempExists {
			_ = parent.Remove(tempName)
		}
	}()

	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		return fmt.Errorf("copy regular file: %w", err)
	}
	if err := out.Chmod(sourceInfo.Mode().Perm()); err != nil {
		out.Close()
		return fmt.Errorf("set destination permissions: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync destination: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}
	if err := parent.Rename(tempName, base); err != nil {
		return fmt.Errorf("replace destination %s: %w", destinationName, err)
	}
	tempExists = false
	return nil
}

func copyDirectory(sourceName string, sourceDir *os.File, sourceInfo os.FileInfo, destination *os.Root, destinationName string) error {
	parentName := filepath.Dir(destinationName)
	if err := destination.MkdirAll(parentName, 0o755); err != nil {
		return fmt.Errorf("create destination parent %s: %w", parentName, err)
	}
	parent, err := destination.OpenRoot(parentName)
	if err != nil {
		return fmt.Errorf("open destination parent %s: %w", parentName, err)
	}
	defer parent.Close()

	base := filepath.Base(destinationName)
	destinationInfo, err := parent.Lstat(base)
	if err == nil {
		if !destinationInfo.IsDir() {
			return fmt.Errorf("destination %s exists and is not a directory", destinationName)
		}
		destinationDir, err := parent.OpenRoot(base)
		if err != nil {
			return fmt.Errorf("open destination directory %s: %w", destinationName, err)
		}
		defer destinationDir.Close()
		return copyDirectoryContents(sourceName, sourceDir, destinationDir)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect destination %s: %w", destinationName, err)
	}

	tempName, err := newCopyTempName()
	if err != nil {
		return err
	}
	if err := parent.Mkdir(tempName, 0o700); err != nil {
		return fmt.Errorf("create temporary destination directory: %w", err)
	}
	tempExists := true
	defer func() {
		if tempExists {
			_ = parent.RemoveAll(tempName)
		}
	}()

	tempRoot, err := parent.OpenRoot(tempName)
	if err != nil {
		return fmt.Errorf("open temporary destination directory: %w", err)
	}
	if err := copyDirectoryContents(sourceName, sourceDir, tempRoot); err != nil {
		tempRoot.Close()
		return err
	}
	if err := tempRoot.Close(); err != nil {
		return fmt.Errorf("close temporary destination directory: %w", err)
	}
	if err := chmodRootDirectory(parent, tempName, sourceInfo.Mode().Perm()); err != nil {
		return err
	}
	if err := parent.Rename(tempName, base); err != nil {
		return fmt.Errorf("install destination directory %s: %w", destinationName, err)
	}
	tempExists = false
	return nil
}

func copyDirectoryContents(sourceName string, sourceDir *os.File, destinationDir *os.Root) error {
	entries, err := sourceDir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read source directory %s: %w", sourceName, err)
	}
	// Reopen the pinned directory descriptor as a Root. Descendants are then
	// resolved relative to that descriptor, so renaming or swapping an ancestor
	// cannot redirect the walk to another tree between ReadDir and OpenFile.
	sourceRoot, err := os.OpenRoot(filepath.Join("/proc/self/fd", strconv.FormatUint(uint64(sourceDir.Fd()), 10)))
	if err != nil {
		return fmt.Errorf("pin source directory %s: %w", sourceName, err)
	}
	defer sourceRoot.Close()
	scopedSource := rootCopySource{root: sourceRoot}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := copyIntoRoot(scopedSource, entry.Name(), destinationDir, entry.Name()); err != nil {
			return fmt.Errorf("copy source %s: %w", filepath.Join(sourceName, entry.Name()), err)
		}
	}
	return nil
}

func chmodRootDirectory(root *os.Root, name string, mode os.FileMode) error {
	dir, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open destination directory for chmod: %w", err)
	}
	defer dir.Close()
	if err := dir.Chmod(mode); err != nil {
		return fmt.Errorf("set destination directory permissions: %w", err)
	}
	return nil
}

func newCopyTempName() (string, error) {
	id, err := generateSessionID()
	if err != nil {
		return "", fmt.Errorf("generate temporary copy name: %w", err)
	}
	return ".waypoint-copy-" + id, nil
}
