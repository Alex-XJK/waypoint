package waypoint

// Full or incremental dumps. With incremental_dumps on, a dump of a fork
// that has a parent image set either extends the image chain of the fork's
// base checkpoint or is a full dump that starts a new chain. It is full once
// the restore time the chain has added (the rent) reaches full_dump_ratio
// times the extra time of a full dump now (the price), or once the chain
// would exceed max_chain_sets. The rent is measured from criu's
// stats-restore; the price is the host's dump time per page written times
// the pages only a full dump would write, counted in the fork's pagemap.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// ChainInfo is a checkpoint's place in its memory image chain and the
// measurements of its dump (incremental_dumps only).
type ChainInfo struct {
	// Sets is the number of image sets a restore reads: 1 for a full dump.
	Sets int `json:"sets"`
	// Root is the checkpoint whose full dump starts the chain.
	Root string `json:"root"`
	// DumpMs is the criu dump wall time. The other measurements are from
	// stats-dump, zero when it could not be read.
	DumpMs       float64 `json:"dump_ms"`
	MemwriteMs   float64 `json:"memwrite_ms,omitempty"`
	PagesScanned uint64  `json:"pages_scanned,omitempty"`
	PagesWritten uint64  `json:"pages_written,omitempty"`
	// Restored is false for a dump of a tree that was never restored (the
	// first checkpoint of a session's main shell).
	Restored bool `json:"restored"`
	// Decision is nil when the fork had no parent image set to chain to.
	Decision *DumpDecision `json:"decision,omitempty"`
}

// DumpDecision is how a dump chose between full and incremental.
type DumpDecision struct {
	Full   bool   `json:"full"`
	Reason string `json:"reason"`
	// RentMs is the restore time the base checkpoint's chain has added
	// (chainRent), compared with the ratio times PriceMs, the estimated
	// extra time of a full dump.
	RentMs  float64 `json:"rent_ms"`
	PriceMs float64 `json:"price_ms"`
	// ResidentPages and DirtyPages are the pages a full and an incremental
	// dump would write: counted in the fork's pagemap (Scanned, in ScanMs)
	// or, without the scan, those of the base checkpoint's dump.
	ResidentPages uint64  `json:"resident_pages"`
	DirtyPages    uint64  `json:"dirty_pages"`
	Scanned       bool    `json:"scanned"`
	ScanMs        float64 `json:"scan_ms,omitempty"`
}

// DumpDecision reasons.
const (
	dumpReasonRatioZero    = "ratio_zero"     // full_dump_ratio is 0
	dumpReasonUnknownChain = "unknown_chain"  // the base checkpoint's chain is not recorded
	dumpReasonMaxSets      = "max_chain_sets" // the chain would exceed max_chain_sets
	dumpReasonRent         = "rent"           // the rent reached the ratio times the price
	dumpReasonNoPrice      = "no_price"       // no dump time per page measured yet; incremental
	dumpReasonBelowPrice   = "below_price"    // incremental
)

// String renders the chain as key=value tokens for the CLI's phases line.
func (c *ChainInfo) String() string {
	full := 0
	if c.Sets == 1 {
		full = 1
	}
	s := fmt.Sprintf("chain_sets=%d full_dump=%d", c.Sets, full)
	if d := c.Decision; d != nil {
		s += fmt.Sprintf(" dump_reason=%s chain_rent_ms=%.1f full_dump_price_ms=%.1f resident_pages=%d dirty_pages=%d",
			d.Reason, d.RentMs, d.PriceMs, d.ResidentPages, d.DirtyPages)
	}
	return s
}

// chainState is what the full-dump decision knows about the chain of a
// fork's base checkpoint and about the fork's memory.
type chainState struct {
	Sets int    // image sets of the base checkpoint; 0 if unknown
	Root string // the chain's full checkpoint
	// RentMs is the restore time the chain has added (chainRent) and
	// PriceMs the estimated extra time of a full dump of the fork now.
	RentMs     float64
	PriceMs    float64
	PriceKnown bool
	// The pages behind the price, as in DumpDecision.
	ResidentPages uint64
	DirtyPages    uint64
	Scanned       bool
	ScanMs        float64
}

// decideFullDump decides whether a dump that would extend the chain s is
// full instead.
func decideFullDump(s chainState, ratio float64, maxSets int) DumpDecision {
	d := DumpDecision{RentMs: s.RentMs, PriceMs: s.PriceMs, ResidentPages: s.ResidentPages, DirtyPages: s.DirtyPages,
		Scanned: s.Scanned, ScanMs: s.ScanMs}
	switch {
	case ratio == 0:
		d.Full, d.Reason = true, dumpReasonRatioZero
	case s.Sets <= 0:
		d.Full, d.Reason = true, dumpReasonUnknownChain
	case s.Sets+1 > maxSets:
		d.Full, d.Reason = true, dumpReasonMaxSets
	case !s.PriceKnown:
		d.Reason = dumpReasonNoPrice
	case s.RentMs >= ratio*s.PriceMs:
		d.Full, d.Reason = true, dumpReasonRent
	default:
		d.Reason = dumpReasonBelowPrice
	}
	return d
}

// pageScan counts the pages a full dump of a fork would write (resident)
// and those an incremental one would (dirty).
type pageScan func() (resident, dirty uint64, err error)

// dumpParent returns the --prev-images-dir for the next dump of f: the image
// set its last restore wrote, or empty when the dump is to be full. It also
// returns the chain and the decision, nil when there was nothing to decide.
func (m *Manager) dumpParent(f *Fork) (string, chainState, *DumpDecision) {
	if f.NextImagesDir == "" || !incrementalDumps() {
		return f.NextImagesDir, chainState{}, nil
	}
	st := m.chainStateFrom(f.BaseCheckpointID, func() (uint64, uint64, error) { return softDirtyPages(f.PID) })
	d := decideFullDump(st, FullDumpRatio, MaxChainSets)
	if d.Full {
		return "", st, &d
	}
	return f.NextImagesDir, st, &d
}

// chainStateFrom walks from the base checkpoint to its chain's full
// checkpoint for the rent of the chain's restores (chainRent; restores by
// any fork count) and prices a full dump of the fork: the host's dump time
// per page written times the pages scan counts as resident and not dirty.
// Without a scan the pages are those of the base checkpoint's dump; without
// the host's time per page, the time is that of the chain's full dump if it
// was of a tree never restored.
func (m *Manager) chainStateFrom(baseID string, scan pageScan) chainState {
	base, err := m.loadMetadata(baseID)
	if err != nil || base.Chain == nil || base.Chain.Sets < 1 {
		return chainState{}
	}
	st := chainState{Sets: base.Chain.Sets, Root: base.Chain.Root}
	if FullDumpRatio == 0 || st.Sets+1 > MaxChainSets {
		return st // decided without the rent and the price
	}
	path := []*Metadata{base} // base first, the full checkpoint last
	for md := base; md.Chain.Sets > 1; {
		if md.ParentID == "" {
			return chainState{}
		}
		parent, err := m.loadMetadata(md.ParentID)
		if err != nil || parent.Chain == nil || parent.Chain.Sets != md.Chain.Sets-1 || parent.Chain.Root != st.Root {
			return chainState{}
		}
		path = append(path, parent)
		md = parent
	}
	costs := readHostCosts(m.hostCostsPath())

	levels := make([][]restoreSample, 0, len(path)-1) // oldest first
	var largest uint64
	for i := len(path) - 2; i >= 0; i-- {
		samples := m.restoreSamples(path[i].ID)
		for _, s := range samples {
			largest = max(largest, s.Pages)
		}
		levels = append(levels, samples)
	}
	restoreMsPerPage, _ := costs.Restore.slope(float64(largest))
	st.RentMs = chainRent(levels, restoreMsPerPage)

	if scan != nil {
		start := time.Now()
		if resident, dirty, err := scan(); err == nil && resident > 0 {
			st.ResidentPages, st.DirtyPages, st.Scanned = resident, min(dirty, resident), true
			st.ScanMs = durMs(time.Since(start))
		}
	}
	if !st.Scanned {
		st.ResidentPages = base.Chain.PagesScanned
		if base.Chain.Sets > 1 {
			st.DirtyPages = min(base.Chain.PagesWritten, st.ResidentPages)
		}
	}
	if st.ResidentPages == 0 {
		return st
	}
	extra := float64(st.ResidentPages - st.DirtyPages)
	root := path[len(path)-1].Chain
	if msPerPage, ok := costs.Dump.slope(extra); ok {
		st.PriceMs, st.PriceKnown = msPerPage*extra, true
	} else if !root.Restored && root.PagesWritten > 0 {
		st.PriceMs, st.PriceKnown = root.DumpMs/float64(root.PagesWritten)*extra, true
	}
	return st
}

// chainRent is the restore time the levels of a chain (the checkpoints
// after its full one, oldest first, each with its restores) have added: the
// time of every restore above the mean of the first restored level's, with
// msPerPage for the difference in pages. A negative sum counts as 0.
func chainRent(levels [][]restoreSample, msPerPage float64) float64 {
	var ref, rent float64
	found := false
	for _, samples := range levels {
		if len(samples) == 0 {
			continue
		}
		if !found {
			for _, s := range samples {
				ref += s.RestoreMs - msPerPage*float64(s.Pages)
			}
			ref /= float64(len(samples))
			found = true
			continue
		}
		for _, s := range samples {
			rent += s.RestoreMs - msPerPage*float64(s.Pages) - ref
		}
	}
	return math.Max(rent, 0)
}

// newChainInfo records the dump of fork f into checkpointID: an incremental
// dump extends the chain st, a full one starts a chain.
func (m *Manager) newChainInfo(f *Fork, checkpointID string, st chainState, incremental bool, dumpMs float64, d *DumpDecision) *ChainInfo {
	c := &ChainInfo{Sets: 1, Root: checkpointID, DumpMs: dumpMs, Restored: f.BaseCheckpointID != "", Decision: d}
	if incremental {
		c.Sets, c.Root = st.Sets+1, st.Root
	}
	if ds, err := readCriuDumpStats(filepath.Join(m.checkpointCriuDir(checkpointID), "stats-dump")); err == nil {
		c.MemwriteMs, c.PagesScanned, c.PagesWritten = ds.MemwriteMs, ds.PagesScanned, ds.PagesWritten
	}
	return c
}

// --- restores ---

// restoreStatsFile, in a checkpoint's dir, has one line per restore of the
// checkpoint with criu's stats-restore numbers.
const restoreStatsFile = "restore-stats.jsonl"

type restoreSample struct {
	Fork      string  `json:"fork"`
	RestoreMs float64 `json:"restore_ms"` // criu restore_time
	Pages     uint64  `json:"pages"`      // pages_restored
	At        int64   `json:"at"`
}

// recordRestore appends a restore of checkpointID to its restore-stats file.
// Forks of one checkpoint append concurrently, each line in one O_APPEND
// write.
func (m *Manager) recordRestore(checkpointID, forkID string, rs *CriuRestoreStats) {
	line, err := json.Marshal(restoreSample{Fork: forkID, RestoreMs: rs.RestoreMs, Pages: rs.PagesRestored, At: time.Now().Unix()})
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(filepath.Join(m.checkpointDir(checkpointID), restoreStatsFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: restore of checkpoint %s not recorded: %v\n", checkpointID, err)
	}
}

// restoreSamples reads the restore-stats file of checkpointID. Lines that do
// not parse (an append in progress) are skipped.
func (m *Manager) restoreSamples(checkpointID string) []restoreSample {
	f, err := os.Open(filepath.Join(m.checkpointDir(checkpointID), restoreStatsFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []restoreSample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s restoreSample
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// --- host costs ---

// hostCostsFile, prefixed with a dot in the sessions dir, holds the dump and
// restore times measured by every session there.
const hostCostsFile = "chain-costs.json"

// hostCosts are the times per page measured on this host.
type hostCosts struct {
	// Dump is the dump time over the pages written, from the dumps (full
	// and incremental) of restored trees.
	Dump costLine `json:"dump"`
	// Restore is the restore time over the pages restored, from restores of
	// full and first-level checkpoints.
	Restore costLine `json:"restore"`
}

// costLine holds the sums of samples (pages, ms) for the least-squares line
// of ms over pages.
type costLine struct {
	N        float64 `json:"n"`
	SumX     float64 `json:"sum_x"`
	SumY     float64 `json:"sum_y"`
	SumXX    float64 `json:"sum_xx"`
	SumXY    float64 `json:"sum_xy"`
	MinPages float64 `json:"min_pages"`
	MaxPages float64 `json:"max_pages"`
}

const (
	// costLineSamples is the weight a line holds: at it, the sums are
	// halved before the next sample.
	costLineSamples = 1024
	// costLineReach is how many times the largest sample's pages a line's
	// slope is used for.
	costLineReach = 4
)

func (l *costLine) add(pages, ms float64) {
	if ms <= 0 || pages < 0 {
		return
	}
	if l.N == 0 || pages < l.MinPages {
		l.MinPages = pages
	}
	l.MaxPages = math.Max(l.MaxPages, pages)
	if l.N >= costLineSamples {
		l.N, l.SumX, l.SumY, l.SumXX, l.SumXY = l.N/2, l.SumX/2, l.SumY/2, l.SumXX/2, l.SumXY/2
	}
	l.N++
	l.SumX += pages
	l.SumY += ms
	l.SumXX += pages * pages
	l.SumXY += pages * ms
}

// slope is the line's ms per page, for an operation on pages pages. It is
// unknown with fewer than two samples, when the smallest sample is above
// half the largest, when pages is beyond costLineReach times the largest,
// and when the slope is not positive.
func (l costLine) slope(pages float64) (float64, bool) {
	den := l.N*l.SumXX - l.SumX*l.SumX
	if l.N < 2 || den <= 0 || l.MinPages > l.MaxPages/2 || pages > costLineReach*l.MaxPages {
		return 0, false
	}
	b := (l.N*l.SumXY - l.SumX*l.SumY) / den
	return b, b > 0
}

func (m *Manager) hostCostsPath() string {
	return filepath.Join(filepath.Dir(m.baseDir), "."+hostCostsFile)
}

func readHostCosts(path string) hostCosts {
	var c hostCosts
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

// recordChainCosts adds the dump of checkpointID, if of a restored tree,
// and, for a full or first-level checkpoint, its restores so far to the
// host's costs. The caller holds the session lock; sessions sharing the
// file overwrite each other's updates at worst.
func (m *Manager) recordChainCosts(checkpointID string, c *ChainInfo) {
	if c == nil {
		return
	}
	var samples []restoreSample
	if c.Sets <= 2 {
		samples = m.restoreSamples(checkpointID)
	}
	dump := c.Restored && c.PagesScanned > 0
	if !dump && len(samples) == 0 {
		return
	}
	path := m.hostCostsPath()
	costs := readHostCosts(path)
	if dump {
		costs.Dump.add(float64(c.PagesWritten), c.DumpMs)
	}
	for _, s := range samples {
		costs.Restore.add(float64(s.Pages), s.RestoreMs)
	}
	data, err := json.Marshal(costs)
	if err == nil {
		err = atomicWriteFile(path, data, 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: dump and restore times not recorded in %s: %v\n", path, err)
	}
}
