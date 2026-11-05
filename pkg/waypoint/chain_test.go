package waypoint

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecideFullDump(t *testing.T) {
	known := func(sets int, overhead, price float64) chainState {
		return chainState{Sets: sets, Root: "c1", OverheadMs: overhead, PriceMs: price, PriceKnown: true}
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
		{"overhead reaches the price", known(5, 500, 500), 1, 128, true, dumpReasonOverhead},
		{"ratio scales the price", known(5, 500, 500), 2, 128, false, dumpReasonBelowPrice},
		{"small ratio", known(5, 250, 500), 0.5, 128, true, dumpReasonOverhead},
		{"ratio zero", known(2, -10, 500), 0, 128, true, dumpReasonRatioZero},
		{"negative overhead stays incremental", known(5, -3, 500), 1, 128, false, dumpReasonBelowPrice},
		{"price at or below zero", known(5, 0, -20), 1, 128, true, dumpReasonOverhead},
		{"unknown chain", chainState{}, 1, 128, true, dumpReasonUnknownChain},
		{"cap", known(16, 0, 500), 1, 16, true, dumpReasonMaxSets},
		{"one below the cap", known(15, 0, 500), 1, 16, false, dumpReasonBelowPrice},
		{"max one set", known(1, 0, 500), 1, 1, true, dumpReasonMaxSets},
		{"no price", chainState{Sets: 5, Root: "c1", OverheadMs: 1e6}, 1, 128, false, dumpReasonNoPrice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decideFullDump(tc.state, tc.ratio, tc.maxSets)
			if d.Full != tc.wantFull || d.Reason != tc.wantReason {
				t.Fatalf("decideFullDump(%+v, %g, %d) = %+v, want full=%v reason=%s",
					tc.state, tc.ratio, tc.maxSets, d, tc.wantFull, tc.wantReason)
			}
			if d.OverheadMs != tc.state.OverheadMs || d.PriceMs != tc.state.PriceMs {
				t.Fatalf("decision %+v does not carry the chain's overhead and price", d)
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
	m.recordFullDump(c)
	if err := m.saveMetadata(id, Metadata{ID: id, ParentID: parent, Chain: c, Status: CheckpointStatusReady}); err != nil {
		t.Fatalf("saveMetadata(%s): %v", id, err)
	}
	for _, ms := range restoreMs {
		m.recordRestore(id, "f", &CriuRestoreStats{RestoreMs: ms, PagesRestored: pages})
	}
}

func TestChainStateFrom(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	const pages = 1000
	// c1: full dump of a restored tree, 0.6 ms per page; restores at 0.2 ms per page.
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 600, MemwriteMs: 400,
		PagesScanned: pages, PagesWritten: pages, Restored: true}, pages, 190, 210)
	// c2: the first incremental dump; one restore 10 ms above the base.
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 90, MemwriteMs: 50,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 210)
	// c3: memwrite 10 ms above c2's for the same pages, two restores 20 ms above the base.
	saveChainCheckpoint(t, m, "c3", "c2", &ChainInfo{Sets: 3, Root: "c1", DumpMs: 95, MemwriteMs: 60,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 220, 220)

	st := m.chainStateFrom("c3")
	want := chainState{Sets: 3, Root: "c1", OverheadMs: 10 + 0 + 2*20 + 10, PriceMs: 0.6*pages - 95, PriceKnown: true}
	if math.Abs(st.OverheadMs-want.OverheadMs) > 1e-9 || math.Abs(st.PriceMs-want.PriceMs) > 1e-9 {
		t.Fatalf("chainStateFrom(c3) = %+v, want %+v", st, want)
	}
	st.OverheadMs, st.PriceMs = want.OverheadMs, want.PriceMs
	if st != want {
		t.Fatalf("chainStateFrom(c3) = %+v, want %+v", st, want)
	}

	// From the full checkpoint itself: no overhead, and no incremental dump to subtract.
	if st := m.chainStateFrom("c1"); st.Sets != 1 || st.OverheadMs != 0 || st.PriceMs != 600 {
		t.Fatalf("chainStateFrom(c1) = %+v, want 1 set, no overhead, price 600", st)
	}

	// Faster restores give a negative overhead, kept as it is.
	saveChainCheckpoint(t, m, "c4", "c3", &ChainInfo{Sets: 4, Root: "c1", DumpMs: 95, MemwriteMs: 60,
		PagesScanned: pages, PagesWritten: 100, Restored: true}, pages, 100)
	if st := m.chainStateFrom("c4"); math.Abs(st.OverheadMs-(want.OverheadMs+10-100)) > 1e-9 {
		t.Fatalf("chainStateFrom(c4).OverheadMs = %g, want %g", st.OverheadMs, want.OverheadMs+10-100)
	}

	// A chain the walk cannot follow is unknown.
	if err := m.saveMetadata("old", Metadata{ID: "old", Status: CheckpointStatusReady}); err != nil {
		t.Fatalf("saveMetadata: %v", err)
	}
	saveChainCheckpoint(t, m, "orphan", "old", &ChainInfo{Sets: 2, Root: "old", Restored: true}, pages)
	saveChainCheckpoint(t, m, "gap", "c2", &ChainInfo{Sets: 4, Root: "c1", Restored: true}, pages)
	for _, id := range []string{"old", "orphan", "gap", "missing"} {
		if st := m.chainStateFrom(id); st.Sets != 0 {
			t.Errorf("chainStateFrom(%s) = %+v, want an unknown chain", id, st)
		}
	}
}

func TestChainStateFromWithoutMeasurements(t *testing.T) {
	setChainKnobs(t, 1, 128)
	m := newManager(t.TempDir())
	// No stats at all: no overhead, no price; the decision is incremental up to the cap.
	saveChainCheckpoint(t, m, "c1", "", &ChainInfo{Sets: 1, Root: "c1", DumpMs: 600, Restored: true}, 0)
	saveChainCheckpoint(t, m, "c2", "c1", &ChainInfo{Sets: 2, Root: "c1", DumpMs: 90, Restored: true}, 0)
	st := m.chainStateFrom("c2")
	if st.Sets != 2 || st.OverheadMs != 0 || st.PriceKnown {
		t.Fatalf("chainStateFrom(c2) without stats = %+v, want 2 sets, no overhead, no price", st)
	}
	if d := decideFullDump(st, 1, 128); d.Full || d.Reason != dumpReasonNoPrice {
		t.Fatalf("decision without stats = %+v, want incremental for lack of a price", d)
	}
}

// simulateChain makes steps checkpoints of one lineage the way snapshotFork
// records them, without criu. Every checkpoint is restored restores times;
// a restore of a checkpoint with s sets takes 200 + gRestore*(s-1) ms, an
// incremental dump's memwrite grows by gDump per set beyond the first
// incremental one, a full dump of a restored tree takes 610 ms and the
// session's first, fresh one 244 ms (1000 pages each), an incremental one
// 90 ms. noise is added to every restore with alternating signs. It returns
// the sets of every checkpoint and the reasons of the full dumps.
func simulateChain(t *testing.T, steps, restores int, gRestore, gDump, noise float64) ([]int, []string) {
	t.Helper()
	m := newManager(t.TempDir())
	const pages = 1000
	var sets []int
	var reasons []string
	prev, sign := "", 1.0
	for i := 1; i <= steps; i++ {
		id := fmt.Sprintf("c%d", i)
		c := &ChainInfo{Sets: 1, Root: id, DumpMs: 610, MemwriteMs: 400, PagesScanned: pages, PagesWritten: pages, Restored: prev != ""}
		if prev == "" {
			c.DumpMs = 244
		} else {
			st := m.chainStateFrom(prev)
			d := decideFullDump(st, FullDumpRatio, MaxChainSets)
			c.Decision = &d
			if d.Full {
				reasons = append(reasons, d.Reason)
			} else {
				c.Sets, c.Root = st.Sets+1, st.Root
				c.DumpMs, c.MemwriteMs, c.PagesWritten = 90, 40+gDump*float64(c.Sets-2), 50
			}
		}
		var times []float64
		for r := 0; r < restores; r++ {
			times = append(times, 200+gRestore*float64(c.Sets-1)+sign*noise)
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

// wantCycle is the number of incremental dumps after which the rule fires:
// the first k with R*gRestore*k(k+1)/2 + gDump*k(k-1)/2 >= ratio*price.
func wantCycle(restores int, gRestore, gDump, ratio, price float64) int {
	for k := 1; ; k++ {
		fk := float64(k)
		if float64(restores)*gRestore*fk*(fk+1)/2+gDump*fk*(fk-1)/2 >= ratio*price {
			return k
		}
	}
}

func TestFullDumpPeriod(t *testing.T) {
	const fullPrice, freshPrice = 610 - 90, 244 - 90
	cases := []struct {
		name     string
		ratio    float64
		restores int
	}{
		{"ratio 1", 1, 1},
		{"ratio 2", 2, 1},
		{"ratio 0.5", 0.5, 1},
		{"four restores per checkpoint", 1, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setChainKnobs(t, tc.ratio, 128)
			const gRestore, gDump = 6.5, 2.8
			sets, reasons := simulateChain(t, 80, tc.restores, gRestore, gDump, 0)
			got := cycles(sets)
			if len(got) < 3 {
				t.Fatalf("full dumps after %v incremental ones, want at least three cycles; sets %v", got, sets)
			}
			for _, r := range reasons {
				if r != dumpReasonOverhead {
					t.Fatalf("full dump reasons %v, want only %s", reasons, dumpReasonOverhead)
				}
			}
			// Until a full dump of a restored tree is measured, the price is
			// from the session's first dump and the first cycle is shorter.
			if want := wantCycle(tc.restores, gRestore, gDump, tc.ratio, freshPrice); got[0] != want {
				t.Errorf("first cycle %d incremental dumps, want %d (price from the fresh dump); cycles %v", got[0], want, got)
			}
			want := wantCycle(tc.restores, gRestore, gDump, tc.ratio, fullPrice)
			for _, k := range got[1:] {
				if k != want {
					t.Errorf("cycle of %d incremental dumps, want %d; cycles %v", k, want, got)
				}
			}
			// The continuous optimum sqrt(2cC/g), g = R*gRestore + gDump.
			g := float64(tc.restores)*gRestore + gDump
			if opt := math.Sqrt(2 * tc.ratio * fullPrice / g); math.Abs(float64(want)-opt) > 1.5 {
				t.Errorf("cycle %d incremental dumps, more than 1.5 from sqrt(2cC/g) = %.2f", want, opt)
			}
		})
	}
}

func TestFullDumpCapWhenChainCostsNothing(t *testing.T) {
	setChainKnobs(t, 1, 12)
	// No growth, restores alternating 3 ms above and below the base.
	sets, reasons := simulateChain(t, 40, 2, 0, 0, 3)
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

func TestFullDumpRatioZeroMakesEveryDumpFull(t *testing.T) {
	setChainKnobs(t, 0, 128)
	sets, _ := simulateChain(t, 5, 1, 6.5, 2.8, 0)
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

func TestFullDumpRate(t *testing.T) {
	sessions := t.TempDir()
	m := newManager(filepath.Join(sessions, "s1"))
	other := newManager(filepath.Join(sessions, "s2"))
	if _, ok := m.fullDumpMsPerPage(); ok {
		t.Fatalf("a new session knows a full-dump time")
	}

	// The first, fresh dump is the session's provisional value only.
	m.recordFullDump(&ChainInfo{Sets: 1, DumpMs: 244, PagesScanned: 1000})
	if v, ok := m.fullDumpMsPerPage(); !ok || v != 0.244 {
		t.Fatalf("after a fresh dump: %g, %v; want 0.244", v, ok)
	}
	if _, ok := other.fullDumpMsPerPage(); ok {
		t.Fatalf("a fresh dump reached the host's rate")
	}

	// A restored tree's full dump replaces it and reaches the host's rate.
	m.recordFullDump(&ChainInfo{Sets: 1, DumpMs: 600, PagesScanned: 1000, Restored: true})
	if v, ok := m.fullDumpMsPerPage(); !ok || v != 0.6 {
		t.Fatalf("after a restored dump: %g, %v; want 0.6", v, ok)
	}
	if v, ok := other.fullDumpMsPerPage(); !ok || v != 0.6 {
		t.Fatalf("another session on the host: %g, %v; want 0.6", v, ok)
	}

	// A moving average with the newest sample weighted fullDumpRateWeight.
	m.recordFullDump(&ChainInfo{Sets: 1, DumpMs: 700, PagesScanned: 1000, Restored: true})
	want := 0.6 + fullDumpRateWeight*(0.7-0.6)
	if v, _ := m.fullDumpMsPerPage(); math.Abs(v-want) > 1e-12 {
		t.Fatalf("after a second sample: %g, want %g", v, want)
	}

	// The session's own value wins over the host's.
	other.recordFullDump(&ChainInfo{Sets: 1, DumpMs: 1000, PagesScanned: 1000, Restored: true})
	if v, _ := m.fullDumpMsPerPage(); math.Abs(v-want) > 1e-12 {
		t.Fatalf("after another session's dump: %g, want the session's own %g", v, want)
	}

	// Incremental dumps and dumps without stats are not full-dump samples.
	m.recordFullDump(&ChainInfo{Sets: 2, DumpMs: 90, PagesScanned: 1000, Restored: true})
	m.recordFullDump(&ChainInfo{Sets: 1, DumpMs: 90, Restored: true})
	m.recordFullDump(nil)
	if v, _ := m.fullDumpMsPerPage(); math.Abs(v-want) > 1e-12 {
		t.Fatalf("after non-samples: %g, want %g", v, want)
	}
	if _, err := os.Stat(filepath.Join(sessions, "."+fullDumpRateFile)); err != nil {
		t.Fatalf("host rate file: %v", err)
	}
}

func TestChainInfoString(t *testing.T) {
	c := &ChainInfo{Sets: 1, Root: "c4", Decision: &DumpDecision{Full: true, Reason: dumpReasonOverhead, OverheadMs: 530.3, PriceMs: 520}}
	if got, want := c.String(), "chain_sets=1 full_dump=1 dump_reason=overhead chain_overhead_ms=530.3 full_dump_price_ms=520.0"; got != want {
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
		c := &ChainInfo{Sets: i, Root: "c1", DumpMs: 90, MemwriteMs: 40 + float64(i), PagesScanned: pages, PagesWritten: 50, Restored: true}
		if i == 1 {
			c.DumpMs, c.MemwriteMs, c.PagesWritten = 600, 400, pages
		}
		saveChainCheckpoint(tb, m, id, prev, c, pages, times...)
		prev = id
	}
	return prev
}

// BenchmarkChainStateFrom times the decision's walk from a chain's tip,
// four restores per checkpoint.
func BenchmarkChainStateFrom(b *testing.B) {
	for _, sets := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("sets=%d", sets), func(b *testing.B) {
			setChainKnobsTB(b, 1, 1000)
			m := newManager(b.TempDir())
			tip := buildChain(b, m, sets, 4)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if st := m.chainStateFrom(tip); st.Sets != sets {
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

// BenchmarkRecordFullDump times the rate update after a full dump of a
// restored tree (two files, each written and synced).
func BenchmarkRecordFullDump(b *testing.B) {
	m := newManager(filepath.Join(b.TempDir(), "s1"))
	c := &ChainInfo{Sets: 1, DumpMs: 600, PagesScanned: 132000, Restored: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.recordFullDump(c)
	}
}
