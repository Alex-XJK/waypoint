package waypoint

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecideFullDump(t *testing.T) {
	known := func(sets int, rent, price float64) chainState {
		return chainState{Sets: sets, Root: "c1", RentMs: rent, PriceMs: price, PriceKnown: true,
			ResidentPages: 1000, DirtyPages: 50, Scanned: true}
	}
	cases := []struct {
		name       string
		state      chainState
		ratio      float64
		maxSets    int
		wantFull   bool
		wantReason string
	}{
		{"below the price", known(5, 100, 500), 1, 128, false, dumpReasonBelowPrice},
		{"rent reaches the price", known(5, 500, 500), 1, 128, true, dumpReasonRent},
		{"ratio scales the price", known(5, 500, 500), 2, 128, false, dumpReasonBelowPrice},
		{"small ratio", known(5, 250, 500), 0.5, 128, true, dumpReasonRent},
		{"ratio zero", known(2, 0, 500), 0, 128, true, dumpReasonRatioZero},
		{"no rent", known(5, 0, 500), 1, 128, false, dumpReasonBelowPrice},
		{"every page dirty", known(5, 0, 0), 1, 128, true, dumpReasonRent},
		{"every page dirty, large ratio", known(5, 0, 0), 1e6, 128, true, dumpReasonRent},
		{"unknown chain", chainState{}, 1, 128, true, dumpReasonUnknownChain},
		{"cap", known(16, 0, 500), 1, 16, true, dumpReasonMaxSets},
		{"one below the cap", known(15, 0, 500), 1, 16, false, dumpReasonBelowPrice},
		{"max one set", known(1, 0, 500), 1, 1, true, dumpReasonMaxSets},
		{"no price", chainState{Sets: 5, Root: "c1", RentMs: 1e6}, 1, 128, false, dumpReasonNoPrice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideFullDump(tc.state, tc.ratio, tc.maxSets)
			if d.Full != tc.wantFull || d.Reason != tc.wantReason {
				t.Fatalf("decideFullDump(%+v, %g, %d) = %+v, want full=%v reason=%s",
					tc.state, tc.ratio, tc.maxSets, d, tc.wantFull, tc.wantReason)
			}
			if d.RentMs != tc.state.RentMs || d.PriceMs != tc.state.PriceMs ||
				d.ResidentPages != tc.state.ResidentPages || d.DirtyPages != tc.state.DirtyPages || d.Scanned != tc.state.Scanned {
				t.Fatalf("decision %+v does not carry the chain's rent, price and pages", d)
			}
		})
	}
}

// setChainKnobs sets full_dump_ratio and max_chain_sets for one test.
func setChainKnobs(t *testing.T, ratio float64, maxSets int) { setChainKnobsTB(t, ratio, maxSets) }

func setChainKnobsTB(t testing.TB, ratio float64, maxSets int) {
	t.Helper()
	prevRatio, prevSets := FullDumpRatio, MaxChainSets
	t.Cleanup(func() { FullDumpRatio, MaxChainSets = prevRatio, prevSets })
	FullDumpRatio, MaxChainSets = ratio, maxSets
}

// saveChainCheckpoint saves the metadata of a checkpoint with chain c and
// records restores of it, as snapshotFork and restoreForkAndWait do.
func saveChainCheckpoint(t testing.TB, m *Manager, id, parent string, c *ChainInfo, pages uint64, restoreMs ...float64) {
	t.Helper()
	if err := os.MkdirAll(m.checkpointDir(id), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := m.saveMetadata(id, Metadata{ID: id, ParentID: parent, Chain: c, Status: CheckpointStatusReady}); err != nil {
		t.Fatalf("saveMetadata(%s): %v", id, err)
	}
	for _, ms := range restoreMs {
		m.recordRestore(id, "f", &CriuRestoreStats{RestoreMs: ms, PagesRestored: pages})
	}
	m.recordChainCosts(id, c)
}

// scanOf is a pageScan that counts resident and dirty pages.
func scanOf(resident, dirty uint64) pageScan {
	return func() (uint64, uint64, error) { return resident, dirty, nil }
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestChainStateFrom(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	const pages = 1000
	// Dumps of the restored tree take 50 ms + 0.5 ms per page written.
	// c1: full; c2, c3, c4: incremental, 100 pages each.
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 550,
		PagesScanned: pages, PagesWritten: pages, Restored: true}, pages, 190, 210)
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 100,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 205, 215)
	saveChainCheckpoint(t, m, "c3", "c2", &ChainInfo{Sets: 3, Root: "c1", DumpMs: 100,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 220, 220)

	// The first level (c2) restores in 210 ms on average; c3's two restores
	// are 10 ms above it. A full dump writes 800 pages more than the 200
	// dirty ones.
	st := m.chainStateFrom("c3", scanOf(pages, 200))
	if !st.Scanned || st.ScanMs < 0 {
		t.Fatalf("chainStateFrom(c3) = %+v, want a scan", st)
	}
	st.ScanMs = 0
	want := chainState{Sets: 3, Root: "c1", RentMs: 20, PriceMs: 400, PriceKnown: true,
		ResidentPages: pages, DirtyPages: 200, Scanned: true}
	if !near(st.RentMs, want.RentMs) || !near(st.PriceMs, want.PriceMs) {
		t.Fatalf("chainStateFrom(c3) = %+v, want %+v", st, want)
	}
	st.RentMs, st.PriceMs = want.RentMs, want.PriceMs
	if st != want {
		t.Fatalf("chainStateFrom(c3) = %+v, want %+v", st, want)
	}

	// Without a scan, or when it fails or finds nothing, the pages are those
	// of the base checkpoint's dump.
	for name, scan := range map[string]pageScan{
		"none":    nil,
		"failing": func() (uint64, uint64, error) { return 0, 0, errors.New("no PAGEMAP_SCAN") },
		"empty":   scanOf(0, 0),
	} {
		st := m.chainStateFrom("c3", scan)
		if st.Scanned || st.ResidentPages != pages || st.DirtyPages != 100 || !near(st.PriceMs, 450) || !near(st.RentMs, 20) {
			t.Fatalf("chainStateFrom(c3) with scan %s = %+v, want the base's 1000 and 100 pages, price 450", name, st)
		}
	}

	// From the full checkpoint itself: no rent, and no dirty pages known.
	if st := m.chainStateFrom("c1", nil); st.Sets != 1 || st.RentMs != 0 || st.DirtyPages != 0 || !near(st.PriceMs, 500) {
		t.Fatalf("chainStateFrom(c1) = %+v, want 1 set, no rent, price 500", st)
	}
	// From the first level: it is the reference, so no rent yet.
	if st := m.chainStateFrom("c2", nil); st.Sets != 2 || st.RentMs != 0 {
		t.Fatalf("chainStateFrom(c2) = %+v, want 2 sets, no rent", st)
	}

	// Restores faster than the first level's do not make the rent negative.
	saveChainCheckpoint(t, m, "c4", "c3", &ChainInfo{Sets: 4, Root: "c1", DumpMs: 100,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 100)
	if st := m.chainStateFrom("c4", nil); st.RentMs != 0 {
		t.Fatalf("chainStateFrom(c4).RentMs = %g, want 0", st.RentMs)
	}

	// A scan with more dirty than resident pages prices a full dump at 0.
	if st := m.chainStateFrom("c3", scanOf(500, 600)); st.DirtyPages != 500 || st.PriceMs != 0 || !st.PriceKnown {
		t.Fatalf("chainStateFrom(c3) with 600 of 500 pages dirty = %+v, want 500 dirty, price 0", st)
	}

	// The ratio 0 and the cap decide without the walk and the scan.
	scanned := false
	scan := func() (uint64, uint64, error) { scanned = true; return pages, 0, nil }
	setChainKnobs(t, 0, 128)
	if st := m.chainStateFrom("c3", scan); st.Sets != 3 || st.PriceKnown || scanned {
		t.Fatalf("chainStateFrom(c3) with ratio 0 = %+v (scanned %v), want 3 sets and nothing else", st, scanned)
	}
	setChainKnobs(t, 1, 3)
	if st := m.chainStateFrom("c3", scan); st.Sets != 3 || st.PriceKnown || scanned {
		t.Fatalf("chainStateFrom(c3) at the cap = %+v (scanned %v), want 3 sets and nothing else", st, scanned)
	}
	setChainKnobs(t, 1, 128)

	// A chain the walk cannot follow is unknown.
	if err := m.saveMetadata("old", Metadata{ID: "old", Status: CheckpointStatusReady}); err != nil {
		t.Fatalf("saveMetadata: %v", err)
	}
	saveChainCheckpoint(t, m, "orphan", "old", &ChainInfo{Sets: 2, Root: "old", Restored: true}, pages)
	saveChainCheckpoint(t, m, "gap", "c2", &ChainInfo{Sets: 4, Root: "c1", Restored: true}, pages)
	for _, id := range []string{"old", "orphan", "gap", "missing"} {
		if st := m.chainStateFrom(id, nil); st.Sets != 0 {
			t.Errorf("chainStateFrom(%s) = %+v, want an unknown chain", id, st)
		}
	}
}

func TestChainStateFromGrownTree(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	// Restores take 10 ms + 0.1 ms per page. An earlier chain of this host
	// gave the line: a full checkpoint of 100 pages and one of 2000.
	saveChainCheckpoint(t, m, "a1", "", &ChainInfo{Sets: 1, Root: "a1", DumpMs: 100,
		PagesScanned: 100, PagesWritten: 100, Restored: true}, 100, 20)
	saveChainCheckpoint(t, m, "b1", "", &ChainInfo{Sets: 1, Root: "b1", DumpMs: 1050,
		PagesScanned: 2000, PagesWritten: 2000, Restored: true}, 2000, 210)
	// c1..c4: the tree grows from 100 to 1100 pages at c3; c4's restore is
	// 7 ms above a first level's of its size.
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 100,
		PagesScanned: 100, PagesWritten: 100, Restored: true}, 100, 20)
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 55,
		PagesScanned: 100, PagesWritten: 10, Restored: true}, 100, 20)
	saveChainCheckpoint(t, m, "c3", "c2", &ChainInfo{Sets: 3, Root: "c1", DumpMs: 555,
		PagesScanned: 1100, PagesWritten: 1010, Restored: true}, 1100, 120)
	saveChainCheckpoint(t, m, "c4", "c3", &ChainInfo{Sets: 4, Root: "c1", DumpMs: 55,
		PagesScanned: 1100, PagesWritten: 10, Restored: true}, 1100, 127)
	if st := m.chainStateFrom("c4", nil); !near(st.RentMs, 7) {
		t.Fatalf("chainStateFrom(c4).RentMs = %g, want 7: the growth is not rent", st.RentMs)
	}

	// Without the host's line the growth reads as rent.
	if err := os.Remove(m.hostCostsPath()); err != nil {
		t.Fatalf("remove host costs: %v", err)
	}
	if st := m.chainStateFrom("c4", nil); !near(st.RentMs, 100+107) {
		t.Fatalf("chainStateFrom(c4).RentMs without the host's costs = %g, want 207", st.RentMs)
	}
}

func TestChainStateFromFreshRoot(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	const pages = 1000
	// c1 is the session's first dump, of a tree never restored: until a dump
	// of a restored tree as large is measured, its time per page prices a
	// full dump.
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 244,
		PagesScanned: pages, PagesWritten: pages}, pages, 200)
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 90,
		PagesScanned: pages, PagesWritten: 50, Restored: true}, pages, 210)
	saveChainCheckpoint(t, m, "c3", "c2", &ChainInfo{Sets: 3, Root: "c1", DumpMs: 90,
		PagesScanned: pages, PagesWritten: 50, Restored: true}, pages, 215)
	if st := m.chainStateFrom("c3", scanOf(pages, 50)); !st.PriceKnown || !near(st.PriceMs, 0.244*950) {
		t.Fatalf("chainStateFrom(c3) = %+v, want the price 231.8 from the fresh dump", st)
	}
}

func TestChainStateFromWithoutMeasurements(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	// No stats at all: no rent, no price; the decision is incremental up to the cap.
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 600, Restored: true}, 0)
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 90, Restored: true}, 0)
	st := m.chainStateFrom("c2", nil)
	if st.Sets != 2 || st.RentMs != 0 || st.PriceKnown {
		t.Fatalf("chainStateFrom(c2) without stats = %+v, want 2 sets, no rent, no price", st)
	}
	if d := decideFullDump(st, 1, 128); d.Full || d.Reason != dumpReasonNoPrice {
		t.Fatalf("decision without stats = %+v, want incremental for lack of a price", d)
	}
	// A scan alone does not give a price: the host has no dump time per page.
	if st := m.chainStateFrom("c2", scanOf(1000, 50)); st.PriceKnown || !st.Scanned {
		t.Fatalf("chainStateFrom(c2) with a scan only = %+v, want the pages and no price", st)
	}
}

func TestChainRent(t *testing.T) {
	r := func(pages uint64, ms ...float64) []restoreSample {
		var out []restoreSample
		for _, v := range ms {
			out = append(out, restoreSample{RestoreMs: v, Pages: pages})
		}
		return out
	}
	cases := []struct {
		name      string
		levels    [][]restoreSample
		msPerPage float64
		want      float64
	}{
		{"no levels", nil, 0, 0},
		{"first level only", [][]restoreSample{r(1000, 230, 240)}, 0, 0},
		{"growth above the first level", [][]restoreSample{r(1000, 230, 240), r(1000, 245), r(1000, 255, 265)}, 0, 10 + 20 + 30},
		{"first level never restored", [][]restoreSample{nil, r(1000, 240), r(1000, 250)}, 0, 10},
		{"a level never restored", [][]restoreSample{r(1000, 240), nil, r(1000, 250)}, 0, 10},
		{"faster restores", [][]restoreSample{r(1000, 240), r(1000, 200)}, 0, 0},
		{"larger tree", [][]restoreSample{r(1000, 240), r(3000, 445)}, 0.1, 5},
		{"smaller tree", [][]restoreSample{r(3000, 440), r(1000, 245)}, 0.1, 5},
		{"larger tree, no time per page", [][]restoreSample{r(1000, 240), r(3000, 445)}, 0, 205},
	}
	for _, tc := range cases {
		if got := chainRent(tc.levels, tc.msPerPage); !near(got, tc.want) {
			t.Errorf("%s: chainRent = %g, want %g", tc.name, got, tc.want)
		}
	}
}

func TestCostLine(t *testing.T) {
	var l costLine
	if _, ok := l.slope(1000); ok {
		t.Fatalf("an empty line has a slope")
	}
	l.add(1000, 550)
	if _, ok := l.slope(1000); ok {
		t.Fatalf("a line of one sample has a slope")
	}
	l.add(900, 500)
	if _, ok := l.slope(1000); ok {
		t.Fatalf("a line of samples of similar size has a slope")
	}
	l.add(100, 100)
	l.add(0, 50)
	if b, ok := l.slope(1000); !ok || !near(b, 0.5) {
		t.Fatalf("slope = %g, %v; want 0.5", b, ok)
	}
	if _, ok := l.slope(4000); !ok {
		t.Fatalf("no slope at %d times the largest sample", costLineReach)
	}
	if _, ok := l.slope(4001); ok {
		t.Fatalf("a slope beyond %d times the largest sample", costLineReach)
	}
	// Samples without a time or with negative pages are not counted.
	before := l
	l.add(500, 0)
	l.add(-1, 100)
	if l != before {
		t.Fatalf("line changed by samples that do not count: %+v", l)
	}

	// Times that fall with the pages give no slope.
	var down costLine
	down.add(100, 300)
	down.add(1000, 200)
	if _, ok := down.slope(500); ok {
		t.Fatalf("a falling line has a slope")
	}

	// The weight stays bounded and newer samples move the slope.
	var long costLine
	for i := 0; i < 3*costLineSamples; i++ {
		long.add(100, 100)
		long.add(1000, 550)
	}
	if long.N > costLineSamples+1 {
		t.Fatalf("line holds weight %g, above %d", long.N, costLineSamples)
	}
	if b, ok := long.slope(1000); !ok || !near(b, 0.5) {
		t.Fatalf("slope after %d samples = %g, %v; want 0.5", 6*costLineSamples, b, ok)
	}
	for i := 0; i < 4*costLineSamples; i++ {
		long.add(100, 100)
		long.add(1000, 1000)
	}
	if b, _ := long.slope(1000); math.Abs(b-1) > 0.05 {
		t.Fatalf("slope after the host's times changed = %g, want about 1", b)
	}
}

// restoreTime is the time of one restore of the i-th checkpoint (from 1) of
// a simulated lineage, which has sets image sets.
type restoreTime func(i, sets int) float64

// linearRestore takes 200 ms from one image set and 200 + step + g(sets-1)
// from a chain.
func linearRestore(step, g float64) restoreTime {
	return func(_, sets int) float64 {
		if sets == 1 {
			return 200
		}
		return 200 + step + g*float64(sets-1)
	}
}

// simulateChain makes steps checkpoints of one lineage the way snapshotFork
// records them, without criu. Every checkpoint is restored restores times,
// each taking restoreMs. The tree has 1000 pages, 50 of them dirty at every
// dump: a full dump of the restored tree takes 610 ms and the session's
// first, fresh one 244 ms, an incremental one 90 ms. noise is added to
// every restore with alternating signs. It returns the sets of every
// checkpoint and the reasons of the full dumps.
func simulateChain(t *testing.T, steps, restores int, restoreMs restoreTime, noise float64) ([]int, []string) {
	t.Helper()
	m := newManager(t.TempDir())
	const pages, dirty = 1000, 50
	var sets []int
	var reasons []string
	prev, sign := "", 1.0
	for i := 1; i <= steps; i++ {
		id := fmt.Sprintf("c%d", i)
		c := &ChainInfo{Sets: 1, Root: id, DumpMs: 610, PagesScanned: pages, PagesWritten: pages, Restored: prev != ""}
		if prev == "" {
			c.DumpMs = 244
		} else {
			st := m.chainStateFrom(prev, scanOf(pages, dirty))
			d := decideFullDump(st, FullDumpRatio, MaxChainSets)
			c.Decision = &d
			if d.Full {
				reasons = append(reasons, d.Reason)
			} else {
				c.Sets, c.Root = st.Sets+1, st.Root
				c.DumpMs, c.PagesWritten = 90, dirty
			}
		}
		var times []float64
		for r := 0; r < restores; r++ {
			times = append(times, restoreMs(i, c.Sets)+sign*noise)
			sign = -sign
		}
		saveChainCheckpoint(t, m, id, prev, c, pages, times...)
		sets = append(sets, c.Sets)
		prev = id
	}
	return sets, reasons
}

// cycles returns the number of incremental dumps between consecutive full
// dumps, from the sets of every checkpoint.
func cycles(sets []int) []int {
	var out []int
	for i := 1; i < len(sets); i++ {
		if sets[i] == 1 {
			out = append(out, sets[i-1]-1)
		}
	}
	return out
}

// wantCycle is the number of incremental dumps after which the rule fires
// when every level's restores take gRestore longer than the level before:
// level j is (j-1) gRestore above the first, so k levels have added
// restores * gRestore * k(k-1)/2.
func wantCycle(restores int, gRestore, ratio, price float64) int {
	for k := 1; ; k++ {
		if float64(restores)*gRestore*float64(k*(k-1))/2 >= ratio*price {
			return k
		}
	}
}

func TestFullDumpPeriod(t *testing.T) {
	// 950 pages more than an incremental dump, at (610-90)/950 ms per page
	// once a full dump of the restored tree is measured, and at the fresh
	// dump's 0.244 before.
	const fullPrice, freshPrice = 610 - 90, 0.244 * 950
	cases := []struct {
		name     string
		ratio    float64
		restores int
		step     float64
	}{
		{"ratio 1", 1, 1, 0},
		{"ratio 2", 2, 1, 0},
		{"ratio 0.5", 0.5, 1, 0},
		{"four restores per checkpoint", 1, 4, 0},
		{"step and growth", 1, 4, 11},
		{"step and growth, ratio 2", 2, 4, 11},
		{"step and growth, ratio 0.5", 0.5, 4, 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setChainKnobs(t, tc.ratio, 128)
			const gRestore = 6.5
			sets, reasons := simulateChain(t, 120, tc.restores, linearRestore(tc.step, gRestore), 0)
			got := cycles(sets)
			if len(got) < 3 {
				t.Fatalf("full dumps after %v incremental ones, want at least three cycles; sets %v", got, sets)
			}
			for _, r := range reasons {
				if r != dumpReasonRent {
					t.Fatalf("full dump reasons %v, want only %s", reasons, dumpReasonRent)
				}
			}
			// Until a full dump of a restored tree is measured, the price is
			// from the session's first dump and the first cycle is shorter.
			if want := wantCycle(tc.restores, gRestore, tc.ratio, freshPrice); got[0] != want {
				t.Errorf("first cycle %d incremental dumps, want %d (price from the fresh dump); cycles %v", got[0], want, got)
			}
			// The step of any chain restore does not shorten the cycle.
			want := wantCycle(tc.restores, gRestore, tc.ratio, fullPrice)
			for _, k := range got[1:] {
				if k != want {
					t.Errorf("cycle of %d incremental dumps, want %d; cycles %v", k, want, got)
				}
			}
			// A cycle of the full dump and want incremental ones is at most
			// 2.5 checkpoints longer than sqrt(2cC/B), where the time per
			// snapshot is lowest for growth B per level and no step.
			opt := math.Sqrt(2 * tc.ratio * fullPrice / (float64(tc.restores) * gRestore))
			if d := float64(want+1) - opt; d < 0 || d > 2.5 {
				t.Errorf("cycle of %d checkpoints, sqrt(2cC/B) = %.2f", want+1, opt)
			}
		})
	}
}

func TestFullDumpCapWhenChainCostsNothing(t *testing.T) {
	setChainKnobs(t, 1, 12)
	// No growth, restores alternating 3 ms above and below the base.
	sets, reasons := simulateChain(t, 40, 2, linearRestore(0, 0), 3)
	for i, s := range sets {
		if s > 12 {
			t.Fatalf("checkpoint %d has %d sets, above the cap of 12", i+1, s)
		}
	}
	if got := cycles(sets); len(got) == 0 || got[0] != 11 {
		t.Fatalf("cycles %v, want full dumps every 11 incremental ones (12 sets)", got)
	}
	for _, r := range reasons {
		if r != dumpReasonMaxSets {
			t.Fatalf("full dump reasons %v, want only %s", reasons, dumpReasonMaxSets)
		}
	}
}

func TestFullDumpStepOnlyRunsToCap(t *testing.T) {
	setChainKnobs(t, 1, 40)
	// Every restore from a chain is 11 ms slower at any depth, with noise
	// that does not cancel within a level. A full dump would not remove the
	// step from the next chain, so only the cap cuts the chain.
	sets, reasons := simulateChain(t, 100, 3, linearRestore(11, 0), 3)
	if got := cycles(sets); len(got) < 2 || got[0] != 39 || got[1] != 39 {
		t.Fatalf("cycles %v, want full dumps only at the cap (39 incremental dumps)", got)
	}
	for _, r := range reasons {
		if r != dumpReasonMaxSets {
			t.Fatalf("full dump reasons %v, want only %s", reasons, dumpReasonMaxSets)
		}
	}
}

func TestFullDumpAfterCostJump(t *testing.T) {
	setChainKnobs(t, 1, 128)
	// A step and no growth until checkpoint 30, then (images out of the page
	// cache, say) every set beyond the first costs 30 ms more per restore.
	// The chain is cut within two checkpoints.
	const jump = 30
	restore := func(i, sets int) float64 {
		g := 0.0
		if i >= jump {
			g = 30
		}
		return linearRestore(11, g)(i, sets)
	}
	sets, reasons := simulateChain(t, jump+6, 4, restore, 0)
	for i := 1; i < jump-1; i++ {
		if sets[i] == 1 {
			t.Fatalf("full dump at checkpoint %d before the jump; sets %v", i+1, sets)
		}
	}
	cut := -1
	for i := jump - 1; i < len(sets); i++ {
		if sets[i] == 1 {
			cut = i + 1
			break
		}
	}
	if cut < 0 || cut > jump+2 {
		t.Fatalf("first full dump after the jump at checkpoint %d, want by %d; sets %v", cut, jump+2, sets)
	}
	if reasons[0] != dumpReasonRent {
		t.Fatalf("full dump reasons %v, want %s first", reasons, dumpReasonRent)
	}
}

func TestFullDumpWhenMostPagesAreDirty(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	const pages = 1000
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 550,
		PagesScanned: pages, PagesWritten: pages, Restored: true}, pages, 200)
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 100,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 210)
	saveChainCheckpoint(t, m, "c3", "c2", &ChainInfo{Sets: 3, Root: "c1", DumpMs: 100,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 230)
	// 20 ms of rent: below the price while 100 pages are dirty (450 ms),
	// above it once 970 are (15 ms).
	if d := decideFullDump(m.chainStateFrom("c3", scanOf(pages, 100)), 1, 128); d.Full {
		t.Fatalf("decision with 100 dirty pages = %+v, want incremental", d)
	}
	if d := decideFullDump(m.chainStateFrom("c3", scanOf(pages, 970)), 1, 128); !d.Full || d.Reason != dumpReasonRent {
		t.Fatalf("decision with 970 dirty pages = %+v, want full for the rent", d)
	}
}

func TestFullDumpRatioZeroMakesEveryDumpFull(t *testing.T) {
	setChainKnobs(t, 0, 128)
	sets, _ := simulateChain(t, 5, 1, linearRestore(0, 6.5), 0)
	for i, s := range sets {
		if s != 1 {
			t.Fatalf("checkpoint %d has %d sets with full_dump_ratio 0, want 1", i+1, s)
		}
	}
}

func TestRestoreSamplesSkipPartialLines(t *testing.T) {
	m := newManager(t.TempDir())
	if err := os.MkdirAll(m.checkpointDir("c1"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	m.recordRestore("c1", "a", &CriuRestoreStats{RestoreMs: 201.5, PagesRestored: 7})
	m.recordRestore("c1", "b", &CriuRestoreStats{RestoreMs: 199, PagesRestored: 7})
	f, err := os.OpenFile(filepath.Join(m.checkpointDir("c1"), restoreStatsFile), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = f.WriteString(`{"fork":"c","restore_ms":2`)
	f.Close()
	got := m.restoreSamples("c1")
	if len(got) != 2 || got[0].Fork != "a" || got[0].RestoreMs != 201.5 || got[0].Pages != 7 || got[1].Fork != "b" {
		t.Fatalf("restoreSamples = %+v, want the two complete lines", got)
	}
	if m.restoreSamples("missing") != nil {
		t.Fatalf("restoreSamples of a checkpoint without restores is not empty")
	}
}

func TestRecordChainCosts(t *testing.T) {
	sessions := t.TempDir()
	m := newManager(filepath.Join(sessions, "s1"))
	other := newManager(filepath.Join(sessions, "s2"))
	record := func(id string, c *ChainInfo, pages uint64, restoreMs ...float64) {
		t.Helper()
		saveChainCheckpoint(t, m, id, "", c, pages, restoreMs...)
	}

	// The first, fresh dump is not a dump sample; its restore is a sample.
	record("c1", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 244, PagesScanned: 1000, PagesWritten: 1000}, 1000, 200)
	costs := readHostCosts(m.hostCostsPath())
	if costs.Dump.N != 0 || costs.Restore.N != 1 {
		t.Fatalf("after a fresh dump: %+v, want no dump sample and one restore", costs)
	}

	// Dumps of the restored tree, full or incremental, and the restores of
	// full and first-level checkpoints are samples, for every session.
	record("c2", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 100, PagesScanned: 1000, PagesWritten: 100, Restored: true}, 1000, 210, 212)
	record("c3", &ChainInfo{Sets: 3, Root: "c1", DumpMs: 100, PagesScanned: 1000, PagesWritten: 100, Restored: true}, 1000, 220)
	record("c4", &ChainInfo{Sets: 1, Root: "c4", DumpMs: 550, PagesScanned: 1000, PagesWritten: 1000, Restored: true}, 1000, 200)
	costs = readHostCosts(other.hostCostsPath())
	if costs.Dump.N != 3 || costs.Restore.N != 4 {
		t.Fatalf("after three dumps of the restored tree: %+v, want 3 dump samples and 4 restores", costs)
	}
	if b, ok := costs.Dump.slope(900); !ok || !near(b, 0.5) {
		t.Fatalf("dump ms per page = %g, %v; want 0.5", b, ok)
	}

	// Dumps without stats and a missing chain are not samples.
	record("c5", &ChainInfo{Sets: 3, Root: "c4", DumpMs: 90, Restored: true}, 1000, 215)
	m.recordChainCosts("c6", nil)
	if got := readHostCosts(m.hostCostsPath()); got != costs {
		t.Fatalf("after non-samples: %+v, want %+v", got, costs)
	}
	if _, err := os.Stat(filepath.Join(sessions, "."+hostCostsFile)); err != nil {
		t.Fatalf("host costs file: %v", err)
	}
}

func TestChainInfoString(t *testing.T) {
	c := &ChainInfo{Sets: 1, Root: "c4", Decision: &DumpDecision{Full: true, Reason: dumpReasonRent, RentMs: 610.25, PriceMs: 520,
		ResidentPages: 132000, DirtyPages: 6400, Scanned: true}}
	if got, want := c.String(), "chain_sets=1 full_dump=1 dump_reason=rent chain_rent_ms=610.2 full_dump_price_ms=520.0 resident_pages=132000 dirty_pages=6400"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := (&ChainInfo{Sets: 3, Root: "c1"}).String(); got != "chain_sets=3 full_dump=0" || strings.Contains(got, "reason") {
		t.Fatalf("String() without a decision = %q", got)
	}
}

// buildChain saves a chain c1..c<sets> of 1000-page checkpoints, each
// restored restores times, and returns the tip's ID.
func buildChain(tb testing.TB, m *Manager, sets, restores int) string {
	tb.Helper()
	const pages = 1000
	times := make([]float64, restores)
	for i := range times {
		times[i] = 200 + float64(i)
	}
	prev := ""
	for i := 1; i <= sets; i++ {
		id := fmt.Sprintf("c%d", i)
		c := &ChainInfo{Sets: i, Root: "c1", DumpMs: 90, PagesScanned: pages, PagesWritten: 50, Restored: true}
		if i == 1 {
			c.DumpMs, c.PagesWritten = 600, pages
		}
		saveChainCheckpoint(tb, m, id, prev, c, pages, times...)
		prev = id
	}
	return prev
}

// BenchmarkChainStateFrom times the decision's walk from a chain's tip,
// four restores per checkpoint, without the scan.
func BenchmarkChainStateFrom(b *testing.B) {
	for _, sets := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("sets=%d", sets), func(b *testing.B) {
			setChainKnobsTB(b, 1, 1000)
			m := newManager(b.TempDir())
			tip := buildChain(b, m, sets, 4)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if st := m.chainStateFrom(tip, nil); st.Sets != sets {
					b.Fatalf("chainStateFrom = %+v, want %d sets", st, sets)
				}
			}
		})
	}
}

// BenchmarkRecordRestore times the line appended per restore.
func BenchmarkRecordRestore(b *testing.B) {
	m := newManager(b.TempDir())
	if err := os.MkdirAll(m.checkpointDir("c1"), 0o755); err != nil {
		b.Fatal(err)
	}
	rs := &CriuRestoreStats{RestoreMs: 200, PagesRestored: 132000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.recordRestore("c1", "f", rs)
	}
}

// BenchmarkRecordChainCosts times the update of the host's costs after a
// snapshot (one file, written and synced).
func BenchmarkRecordChainCosts(b *testing.B) {
	m := newManager(filepath.Join(b.TempDir(), "s1"))
	if err := os.MkdirAll(m.checkpointDir("c1"), 0o755); err != nil {
		b.Fatal(err)
	}
	m.recordRestore("c1", "f", &CriuRestoreStats{RestoreMs: 200, PagesRestored: 132000})
	c := &ChainInfo{Sets: 1, DumpMs: 600, PagesScanned: 132000, PagesWritten: 132000, Restored: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.recordChainCosts("c1", c)
	}
}
