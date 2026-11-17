package waypoint

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// skipWithoutPagemapScan skips a test on a kernel without PAGEMAP_SCAN.
func skipWithoutPagemapScan(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) {
		t.Skipf("no PAGEMAP_SCAN on this kernel: %v", err)
	}
}

func TestScanPagemap(t *testing.T) {
	pageSize := os.Getpagesize()
	const pages, touched, rewritten = 1024, 512, 100
	mem, err := unix.Mmap(-1, 0, pages*pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(mem)
	start := uint64(uintptr(unsafe.Pointer(&mem[0])))
	end := start + uint64(len(mem))
	scan := func() (uint64, uint64) {
		t.Helper()
		resident, dirty, err := scanPagemap(os.Getpid(), start, end)
		skipWithoutPagemapScan(t, err)
		if err != nil {
			t.Fatalf("scanPagemap: %v", err)
		}
		return resident, dirty
	}

	// Nothing is resident before the first write; a page only read is the
	// zero page, which no dump writes.
	_ = mem[(pages-1)*pageSize]
	if resident, dirty := scan(); resident != 0 || dirty != 0 {
		t.Fatalf("untouched mapping: %d resident, %d dirty pages, want none", resident, dirty)
	}
	for i := 0; i < touched; i++ {
		mem[i*pageSize] = 1
	}
	resident, dirty := scan()
	if resident != touched {
		t.Fatalf("%d resident pages, want the %d written", resident, touched)
	}
	if dirty == 0 {
		t.Skip("no soft-dirty tracking on this kernel")
	}
	if dirty != touched {
		t.Fatalf("%d dirty pages of a new mapping, want %d", dirty, touched)
	}

	// After the soft-dirty bits are cleared, as criu does at a dump or a
	// restore with --track-mem, only pages written since are dirty.
	if err := os.WriteFile("/proc/self/clear_refs", []byte("4"), 0); err != nil {
		t.Skipf("clear_refs: %v", err)
	}
	if resident, dirty := scan(); resident != touched || dirty != 0 {
		t.Fatalf("after clear_refs: %d resident, %d dirty pages, want %d and 0", resident, dirty, touched)
	}
	// Every fifth page: more regions than one ioctl returns.
	for i := 0; i < rewritten; i++ {
		mem[5*i*pageSize] = 2
	}
	if resident, dirty := scan(); resident != touched || dirty != rewritten {
		t.Fatalf("after rewriting: %d resident, %d dirty pages, want %d and %d", resident, dirty, touched, rewritten)
	}
	// A sub-range counts its own pages only.
	if resident, dirty := func() (uint64, uint64) {
		r, d, err := scanPagemap(os.Getpid(), start, start+uint64(10*pageSize))
		if err != nil {
			t.Fatalf("scanPagemap: %v", err)
		}
		return r, d
	}(); resident != 10 || dirty != 2 {
		t.Fatalf("first 10 pages: %d resident, %d dirty, want 10 and 2", resident, dirty)
	}
}

func TestScanPagemapManyRegions(t *testing.T) {
	pageSize := os.Getpagesize()
	const pages = 8 * pagemapScanRegions
	mem, err := unix.Mmap(-1, 0, pages*pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(mem)
	// Every other page resident: pages/2 regions, several ioctls.
	for i := 0; i < pages; i += 2 {
		mem[i*pageSize] = 1
	}
	start := uint64(uintptr(unsafe.Pointer(&mem[0])))
	resident, _, err := scanPagemap(os.Getpid(), start, start+uint64(len(mem)))
	skipWithoutPagemapScan(t, err)
	if err != nil {
		t.Fatalf("scanPagemap: %v", err)
	}
	if resident != pages/2 {
		t.Fatalf("%d resident pages, want %d", resident, pages/2)
	}
}

func TestMappedRange(t *testing.T) {
	mem, err := unix.Mmap(-1, 0, os.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(mem)
	addr := uint64(uintptr(unsafe.Pointer(&mem[0])))
	start, end, err := mappedRange(os.Getpid())
	if err != nil || start >= end || addr < start || addr >= end {
		t.Fatalf("mappedRange = [%#x, %#x), %v; want a range with %#x", start, end, err, addr)
	}
	if end >= 1<<63 {
		t.Fatalf("mappedRange ends at %#x, in the kernel's half", end)
	}
	if _, _, err := mappedRange(-1); err == nil {
		t.Fatalf("mappedRange of no process succeeded")
	}
}

func TestSoftDirtyPagesOfTree(t *testing.T) {
	if _, err := os.Stat("/proc/self/task/" + strconv.Itoa(unix.Gettid()) + "/children"); err != nil {
		t.Skipf("no children files in /proc: %v", err)
	}
	// A shell with one child.
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	var pids []int
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		if pids, err = treePIDs(cmd.Process.Pid); err != nil {
			t.Fatalf("treePIDs: %v", err)
		}
		if len(pids) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(pids) != 2 || pids[0] != cmd.Process.Pid {
		t.Fatalf("treePIDs = %v, want the shell %d and its child", pids, cmd.Process.Pid)
	}
	comm, _ := os.ReadFile("/proc/" + strconv.Itoa(pids[1]) + "/comm")
	if strings.TrimSpace(string(comm)) != "sleep" {
		t.Fatalf("child %d is %q, want sleep", pids[1], comm)
	}
	if slices.Contains(pids, os.Getpid()) {
		t.Fatalf("treePIDs = %v holds the parent", pids)
	}

	resident, dirty, err := softDirtyPages(cmd.Process.Pid)
	skipWithoutPagemapScan(t, err)
	if err != nil {
		t.Fatalf("softDirtyPages: %v", err)
	}
	if resident == 0 || dirty > resident {
		t.Fatalf("softDirtyPages = %d resident, %d dirty; want some resident pages and no more dirty ones", resident, dirty)
	}
	shell, _, err := softDirtyPages(pids[1])
	if err != nil || shell == 0 || shell >= resident {
		t.Fatalf("softDirtyPages of the child alone = %d, %v; want fewer than the tree's %d", shell, err, resident)
	}

	if _, _, err := softDirtyPages(-1); err == nil {
		t.Fatalf("softDirtyPages of no process succeeded")
	}
}
