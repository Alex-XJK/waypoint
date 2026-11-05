package waypoint

// Full or incremental dumps. With incremental_dumps on, a dump of a fork
// that has a parent image set either extends the image chain of the fork's
// base checkpoint or is a full dump that starts a new chain. It is full once
// one more level would cost more than the chain's average per snapshot,
// counting full_dump_ratio times the estimated extra time of a full dump
// into that average (see replacementStat), or once the chain would exceed
// max_chain_sets. Both sides are measured on this host from criu's
// stats-dump and stats-restore.

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
	// OverheadMs is the replacement statistic of the base checkpoint's chain
	// (replacementStat), compared with the ratio times PriceMs, the
	// estimated extra time of a full dump. RentMs is the overhead the chain
	// has added since its full dump and NextLevelMs the predicted overhead
	// of one more level.
	OverheadMs  float64 `json:"overhead_ms"`
	PriceMs     float64 `json:"price_ms"`
	RentMs      float64 `json:"rent_ms"`
	NextLevelMs float64 `json:"next_level_ms"`
}

// DumpDecision reasons.
const (
	dumpReasonRatioZero    = "ratio_zero"     // full_dump_ratio is 0
	dumpReasonUnknownChain = "unknown_chain"  // the base checkpoint's chain is not recorded
	dumpReasonMaxSets      = "max_chain_sets" // the chain would exceed max_chain_sets
	dumpReasonOverhead     = "overhead"       // the replacement statistic reached the ratio times the price
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
		s += fmt.Sprintf(" dump_reason=%s chain_overhead_ms=%.1f full_dump_price_ms=%.1f chain_rent_ms=%.1f next_level_ms=%.1f",
			d.Reason, d.OverheadMs, d.PriceMs, d.RentMs, d.NextLevelMs)
	}
	return s
}

// chainState is what the full-dump decision knows about the chain of a
// fork's base checkpoint.
type chainState struct {
	Sets int    // image sets of the base checkpoint; 0 if unknown
	Root string // the chain's full checkpoint
	// OverheadMs is the replacement statistic, RentMs the summed overhead
	// of the chain's levels and NextLevelMs the predicted overhead of one
	// more level (replacementStat).
	OverheadMs  float64
	RentMs      float64
	NextLevelMs float64
	PriceMs     float64
	PriceKnown  bool
}

// decideFullDump decides whether a dump that would extend the chain s is
// full instead.
func decideFullDump(s chainState, ratio float64, maxSets int) DumpDecision {
	d := DumpDecision{OverheadMs: s.OverheadMs, PriceMs: s.PriceMs, RentMs: s.RentMs, NextLevelMs: s.NextLevelMs}
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
// checkpoint. Each checkpoint after the full one is a level of the chain;
// its overhead is every restore's time above the full checkpoint's restore
// time per page, plus its dump's memwrite time above the chain's first
// incremental dump's per page written; restores by any fork count. The
// overhead is then the replacement statistic over the levels. The price is
// the full-dump time per page times the base's pages scanned, minus the
// base's own dump time if that dump was incremental.
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
	levels := make([]levelCost, 0, len(path)-1) // oldest first
	for i := len(path) - 2; i >= 0; i-- {
		md := path[i]
		var lc levelCost
		if fullPages > 0 {
			for _, s := range m.restoreSamples(md.ID) {
				lc.RestoreMs += s.RestoreMs - fullMs/fullPages*float64(s.Pages)
				lc.Restores++
			}
		}
		lc.DumpMs = dumpOverheadMs(md.Chain, first)
		levels = append(levels, lc)
	}
	st.RentMs, st.NextLevelMs, st.OverheadMs = replacementStat(levels)

	if msPerPage, ok := m.fullDumpMsPerPage(); ok && base.Chain.PagesScanned > 0 {
		st.PriceKnown = true
		st.PriceMs = msPerPage * float64(base.Chain.PagesScanned)
		if base.Chain.Sets > 1 {
			st.PriceMs -= base.Chain.DumpMs
		}
	}
	return st
}

// levelCost is the overhead one level of a chain has added: the restores of
// its checkpoint above restores of the chain's full checkpoint, and its dump
// above the chain's first incremental dump.
type levelCost struct {
	RestoreMs float64 // summed over its restores
	Restores  int
	DumpMs    float64
}

// replacementStat returns, for the levels 1..k of a chain (oldest first),
// their summed overhead, the predicted overhead of level k+1 and the
// replacement statistic.
//
// A cycle of a full dump (level 0, no overhead) and the incremental levels
// 1..n-1 costs (cC + c_1 + ... + c_{n-1}) / n per snapshot, C being the
// extra time of the full dump and c the ratio. One more level lowers that
// average exactly when its overhead c_n is below it. So level k+1 is better
// a full dump once
//
//	sum_{j=0..k} (c_{k+1} - c_j) >= cC,    c_0 = 0.
//
// The overheads are modelled as a line c_j = A + Bj, j >= 1: the overhead
// per restore fitted over the levels' restores (weighted by their number)
// times the mean number of restores per level, plus the dump overhead
// fitted over the levels, which keeps a level that has not been forked yet
// from reading as a cheap one. With the line, the left side is
// A + B(k+1)(k+2)/2: a constant overhead per level, such as criu's slower
// restore from any chain, counts once, since a full dump does not remove it
// from the next level, and only the growth B makes the chain worth cutting,
// with the period sqrt(2(cC - A)/B). With a single level, A is its overhead
// and B is 0.
func replacementStat(levels []levelCost) (rentMs, nextMs, stat float64) {
	k := len(levels)
	if k == 0 {
		return 0, 0, 0
	}
	var xs, perRestore, restores, dump, ones []float64
	total := 0
	for i, l := range levels {
		j := float64(i + 1)
		rentMs += l.RestoreMs + l.DumpMs
		if l.Restores > 0 {
			xs = append(xs, j)
			perRestore = append(perRestore, l.RestoreMs/float64(l.Restores))
			restores = append(restores, float64(l.Restores))
			total += l.Restores
		}
		dump = append(dump, l.DumpMs)
		ones = append(ones, 1)
	}
	ra, rb := fitLine(xs, perRestore, restores)
	levelXs := make([]float64, k)
	for i := range levelXs {
		levelXs[i] = float64(i + 1)
	}
	da, db := fitLine(levelXs, dump, ones)
	meanRestores := float64(total) / float64(k)
	a, b := meanRestores*ra+da, meanRestores*rb+db
	fk := float64(k)
	return rentMs, a + b*(fk+1), a + b*(fk+1)*(fk+2)/2
}

// fitLine returns the weighted least-squares line y = a + bx; with a single
// distinct x, the weighted mean and slope 0; with no weight, 0 and 0.
func fitLine(xs, ys, ws []float64) (a, b float64) {
	var sw, sx, sy float64
	for i := range xs {
		sw += ws[i]
		sx += ws[i] * xs[i]
		sy += ws[i] * ys[i]
	}
	if sw == 0 {
		return 0, 0
	}
	mx, my := sx/sw, sy/sw
	var sxx, sxy float64
	for i := range xs {
		dx := xs[i] - mx
		sxx += ws[i] * dx * dx
		sxy += ws[i] * dx * (ys[i] - my)
	}
	if sxx == 0 {
		return my, 0
	}
	b = sxy / sxx
	return my - b*mx, b
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
