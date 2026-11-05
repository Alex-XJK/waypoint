package waypoint

// Full or incremental dumps. With incremental_dumps on, a dump of a fork
// that has a parent image set either extends the image chain of the fork's
// base checkpoint or is a full dump that starts a new chain. It is full once
// the overhead the chain has added since its full dump reaches
// full_dump_ratio times the estimated extra time of a full dump, or once the
// chain would exceed max_chain_sets. Both sides are measured on this host
// from criu's stats-dump and stats-restore.

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	// OverheadMs is the overhead the base checkpoint's chain has added since
	// its full dump; PriceMs is the estimated extra time of a full dump.
	OverheadMs float64 `json:"overhead_ms"`
	PriceMs    float64 `json:"price_ms"`
}

// DumpDecision reasons.
const (
	dumpReasonRatioZero    = "ratio_zero"     // full_dump_ratio is 0
	dumpReasonUnknownChain = "unknown_chain"  // the base checkpoint's chain is not recorded
	dumpReasonMaxSets      = "max_chain_sets" // the chain would exceed max_chain_sets
	dumpReasonOverhead     = "overhead"       // the overhead reached the ratio times the price
	dumpReasonNoPrice      = "no_price"       // no full dump measured yet; incremental
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
		s += fmt.Sprintf(" dump_reason=%s chain_overhead_ms=%.1f full_dump_price_ms=%.1f", d.Reason, d.OverheadMs, d.PriceMs)
	}
	return s
}

// chainState is what the full-dump decision knows about the chain of a
// fork's base checkpoint.
type chainState struct {
	Sets       int    // image sets of the base checkpoint; 0 if unknown
	Root       string // the chain's full checkpoint
	OverheadMs float64
	PriceMs    float64
	PriceKnown bool
}

// decideFullDump decides whether a dump that would extend the chain s is
// full instead.
func decideFullDump(s chainState, ratio float64, maxSets int) DumpDecision {
	d := DumpDecision{OverheadMs: s.OverheadMs, PriceMs: s.PriceMs}
	switch {
	case ratio == 0:
		d.Full, d.Reason = true, dumpReasonRatioZero
	case s.Sets <= 0:
		d.Full, d.Reason = true, dumpReasonUnknownChain
	case s.Sets+1 > maxSets:
		d.Full, d.Reason = true, dumpReasonMaxSets
	case !s.PriceKnown:
		d.Reason = dumpReasonNoPrice
	case s.OverheadMs >= ratio*s.PriceMs:
		d.Full, d.Reason = true, dumpReasonOverhead
	default:
		d.Reason = dumpReasonBelowPrice
	}
	return d
}

// dumpParent returns the --prev-images-dir for the next dump of f: the image
// set its last restore wrote, or empty when the dump is to be full. It also
// returns the chain and the decision, nil when there was nothing to decide.
func (m *Manager) dumpParent(f *Fork) (string, chainState, *DumpDecision) {
	if f.NextImagesDir == "" || !incrementalDumps() {
		return f.NextImagesDir, chainState{}, nil
	}
	st := m.chainStateFrom(f.BaseCheckpointID)
	d := decideFullDump(st, FullDumpRatio, MaxChainSets)
	if d.Full {
		return "", st, &d
	}
	return f.NextImagesDir, st, &d
}

// chainStateFrom walks from the base checkpoint to its chain's full
// checkpoint. The overhead sums, over the checkpoints after the full one,
// every restore's time above the full checkpoint's restore time per page
// and every dump's memwrite time above the chain's first incremental dump's
// per page written; restores by any fork count. The price is the full-dump
// time per page times the base's pages scanned, minus the base's own dump
// time if that dump was incremental.
func (m *Manager) chainStateFrom(baseID string) chainState {
	base, err := m.loadMetadata(baseID)
	if err != nil || base.Chain == nil || base.Chain.Sets < 1 {
		return chainState{}
	}
	st := chainState{Sets: base.Chain.Sets, Root: base.Chain.Root}
	if st.Sets+1 > MaxChainSets {
		return st // the cap decides; no need to walk
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

	var fullMs, fullPages float64
	for _, s := range m.restoreSamples(path[len(path)-1].ID) {
		fullMs += s.RestoreMs
		fullPages += float64(s.Pages)
	}
	var first *ChainInfo // the chain's first incremental dump
	if len(path) > 1 {
		first = path[len(path)-2].Chain
	}
	for _, md := range path[:len(path)-1] {
		if fullPages > 0 {
			for _, s := range m.restoreSamples(md.ID) {
				st.OverheadMs += s.RestoreMs - fullMs/fullPages*float64(s.Pages)
			}
		}
		st.OverheadMs += dumpOverheadMs(md.Chain, first)
	}

	if msPerPage, ok := m.fullDumpMsPerPage(); ok && base.Chain.PagesScanned > 0 {
		st.PriceKnown = true
		st.PriceMs = msPerPage * float64(base.Chain.PagesScanned)
		if base.Chain.Sets > 1 {
			st.PriceMs -= base.Chain.DumpMs
		}
	}
	return st
}

// dumpOverheadMs is the memwrite time of dump c above that of the chain's
// first incremental dump, scaled by pages written.
func dumpOverheadMs(c, first *ChainInfo) float64 {
	if first == nil || c == first || c.PagesScanned == 0 || first.PagesScanned == 0 {
		return 0
	}
	if first.PagesWritten == 0 {
		return c.MemwriteMs - first.MemwriteMs
	}
	return c.MemwriteMs - first.MemwriteMs*float64(c.PagesWritten)/float64(first.PagesWritten)
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

// --- full-dump time ---

// fullDumpRateFile holds the measured time per page of full dumps, in the
// session dir for the session and, prefixed with a dot, in the sessions dir
// for every session there.
const fullDumpRateFile = "full-dump-rate.json"

// fullDumpRateWeight is the weight of the newest full dump in MsPerPage.
const fullDumpRateWeight = 0.3

// fullDumpRate is the time per page scanned of full dumps.
type fullDumpRate struct {
	// MsPerPage is a moving average over full dumps of restored trees.
	MsPerPage float64 `json:"ms_per_page"`
	Samples   int     `json:"samples"`
	// FreshMsPerPage is from the session's first full dump, of a tree that
	// was never restored and dumps faster.
	FreshMsPerPage float64 `json:"fresh_ms_per_page,omitempty"`
}

func (m *Manager) sessionDumpRatePath() string {
	return filepath.Join(m.baseDir, fullDumpRateFile)
}

func (m *Manager) hostDumpRatePath() string {
	return filepath.Join(filepath.Dir(m.baseDir), "."+fullDumpRateFile)
}

func readFullDumpRate(path string) fullDumpRate {
	var r fullDumpRate
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &r)
	}
	return r
}

// fullDumpMsPerPage is the time per page of a full dump: the session's
// measurement over restored trees, else the host's, else the session's first
// dump.
func (m *Manager) fullDumpMsPerPage() (float64, bool) {
	session := readFullDumpRate(m.sessionDumpRatePath())
	if session.Samples > 0 {
		return session.MsPerPage, true
	}
	if host := readFullDumpRate(m.hostDumpRatePath()); host.Samples > 0 {
		return host.MsPerPage, true
	}
	return session.FreshMsPerPage, session.FreshMsPerPage > 0
}

// recordFullDump adds the time per page of a full dump c to the session's
// rate and, for a restored tree, to the host's. The caller holds the
// session lock; sessions sharing the host file overwrite each other's
// updates at worst.
func (m *Manager) recordFullDump(c *ChainInfo) {
	if c == nil || c.Sets != 1 || c.PagesScanned == 0 {
		return
	}
	sample := c.DumpMs / float64(c.PagesScanned)
	paths := []string{m.sessionDumpRatePath()}
	if c.Restored {
		paths = append(paths, m.hostDumpRatePath())
	}
	for _, path := range paths {
		r := readFullDumpRate(path)
		switch {
		case !c.Restored:
			r.FreshMsPerPage = sample
		case r.Samples == 0:
			r.MsPerPage, r.Samples = sample, 1
		default:
			r.MsPerPage += fullDumpRateWeight * (sample - r.MsPerPage)
			r.Samples++
		}
		data, err := json.Marshal(r)
		if err == nil {
			err = atomicWriteFile(path, data, 0o644)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: full dump time not recorded in %s: %v\n", path, err)
		}
	}
}
