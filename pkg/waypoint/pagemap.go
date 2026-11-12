package waypoint

// Counts of a process tree's pages from the PAGEMAP_SCAN ioctl (Linux 6.7),
// for the price of a full dump (chain.go).

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// pagemapScan is PAGEMAP_SCAN, _IOWR('f', 16, struct pm_scan_arg).
const pagemapScan = 0xc0606610

// PAGE_IS_* categories of PAGEMAP_SCAN.
const (
	pageIsFile      = 1 << 2
	pageIsPresent   = 1 << 3
	pageIsSwapped   = 1 << 4
	pageIsPFNZero   = 1 << 5
	pageIsSoftDirty = 1 << 7
)

// pmScanArg is struct pm_scan_arg.
type pmScanArg struct {
	size              uint64
	flags             uint64
	start             uint64
	end               uint64
	walkEnd           uint64
	vec               unsafe.Pointer
	vecLen            uint64
	maxPages          uint64
	categoryInverted  uint64
	categoryMask      uint64
	categoryAnyofMask uint64
	returnMask        uint64
}

// pageRegion is struct page_region.
type pageRegion struct {
	start      uint64
	end        uint64
	categories uint64
}

// pagemapScanRegions is the regions asked for per ioctl. Linux 6.8 reports
// some regions twice when asked for more than 512.
const pagemapScanRegions = 256

// softDirtyPages counts, over the process tree rooted at pid, the pages a
// full dump would write (resident: present or swapped, neither file pages
// nor the zero page) and those of them an incremental dump would (dirty:
// soft-dirty, written since the tree's last restore or dump). A process
// that exits meanwhile is skipped.
func softDirtyPages(pid int) (resident, dirty uint64, err error) {
	pids, err := treePIDs(pid)
	if err != nil {
		return 0, 0, err
	}
	for i, p := range pids {
		start, end, err := mappedRange(p)
		if err == nil && end > start {
			var r, d uint64
			r, d, err = scanPagemap(p, start, end)
			resident, dirty = resident+r, dirty+d
		}
		if err != nil && (i == 0 || !isProcessGone(err)) {
			return 0, 0, err
		}
	}
	return resident, dirty, nil
}

func isProcessGone(err error) bool {
	return os.IsNotExist(err) || err == unix.ESRCH
}

// treePIDs returns pid and its descendants, from the children files of
// their threads.
func treePIDs(pid int) ([]int, error) {
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return nil, err
	}
	pids := []int{pid}
	seen := map[int]bool{pid: true}
	for i := 0; i < len(pids); i++ {
		tasks, _ := os.ReadDir(fmt.Sprintf("/proc/%d/task", pids[i]))
		for _, t := range tasks {
			data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pids[i], t.Name()))
			for _, f := range strings.Fields(string(data)) {
				if child, err := strconv.Atoi(f); err == nil && !seen[child] {
					seen[child] = true
					pids = append(pids, child)
				}
			}
		}
	}
	return pids, nil
}

// mappedRange returns the lowest start and the highest end of pid's
// mappings, without [vsyscall], which is not in the user address range.
func mappedRange(pid int) (start, end uint64, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	first := true
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasSuffix(line, "[vsyscall]") {
			continue
		}
		lo, rest, ok := strings.Cut(line, "-")
		hi, _, ok2 := strings.Cut(rest, " ")
		a, err1 := strconv.ParseUint(lo, 16, 64)
		b, err2 := strconv.ParseUint(hi, 16, 64)
		if !ok || !ok2 || err1 != nil || err2 != nil {
			continue
		}
		if first {
			start, first = a, false
		}
		end = max(end, b)
	}
	return start, end, sc.Err()
}

// scanPagemap counts the resident and dirty pages (softDirtyPages) of pid
// in [start, end).
func scanPagemap(pid int, start, end uint64) (resident, dirty uint64, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/pagemap", pid))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	pageSize := uint64(os.Getpagesize())
	var vec [pagemapScanRegions]pageRegion
	arg := pmScanArg{
		size:              uint64(unsafe.Sizeof(pmScanArg{})),
		start:             start,
		end:               end,
		vec:               unsafe.Pointer(&vec[0]),
		vecLen:            pagemapScanRegions,
		categoryInverted:  pageIsFile | pageIsPFNZero,
		categoryMask:      pageIsFile | pageIsPFNZero,
		categoryAnyofMask: pageIsPresent | pageIsSwapped,
		returnMask:        pageIsSoftDirty,
	}
	counted := start // regions below it are counted
	for arg.start < end {
		n, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), pagemapScan, uintptr(unsafe.Pointer(&arg)))
		if errno != 0 {
			return 0, 0, errno
		}
		for _, r := range vec[:n] {
			lo := max(r.start, counted)
			if r.end <= lo {
				continue
			}
			pages := (r.end - lo) / pageSize
			resident += pages
			if r.categories&pageIsSoftDirty != 0 {
				dirty += pages
			}
			counted = r.end
		}
		if arg.walkEnd <= arg.start {
			break
		}
		arg.start = arg.walkEnd
	}
	return resident, dirty, nil
}
