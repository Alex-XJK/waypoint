package waypoint

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// checkDir applies one layer to prevState and builds the record at once: the
// call shape the TestCheckDir_* tests use.
func checkDir(rootViewDir, _, targetPath string, prevState *state, blamed *[]BlameCkptRecord, ckptID string) (bool, error) {
	event, err := advance(prevState, rootViewDir, ckptID, targetPath)
	if err != nil || event == nil {
		return false, err
	}
	if blamed != nil {
		record, err := buildBlameRecord(event, newCompareBufs(), newDiffBudget())
		if err != nil {
			return false, err
		}
		*blamed = append(*blamed, record)
	}
	return true, nil
}

func TestLookupLayer(t *testing.T) {
	const target = "/app/src/main.py"
	cases := []struct {
		name  string
		build func(t *testing.T, layer string)
		want  layerEntry
	}{
		{"empty layer", func(t *testing.T, layer string) {}, layerUntouched},
		{"only directories above the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/other.py"), "x\n")
		}, layerUntouched},
		{"file at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/main.py"), "x\n")
		}, layerPresent},
		{"empty file at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/main.py"), "")
		}, layerPresent},
		{"symlink at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/other.py"), "x\n")
			if err := os.Symlink("other.py", filepath.Join(layer, "app/src/main.py")); err != nil {
				t.Fatal(err)
			}
		}, layerPresent},
		{"directory at the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src/main.py/inner"), "x\n")
		}, layerHidden},
		{"file above the target", func(t *testing.T, layer string) {
			writeTestFile(t, filepath.Join(layer, "app/src"), "x\n")
		}, layerHidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer := t.TempDir()
			tc.build(t, layer)
			got, hostPath, err := lookupLayer(layer, target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("lookupLayer = %d, want %d", got, tc.want)
			}
			if wantPath := filepath.Join(layer, target); got == layerPresent && hostPath != wantPath {
				t.Fatalf("host path = %q, want %q", hostPath, wantPath)
			}
		})
	}
}

// A symlink above the target hides it. The lookup must not walk through the
// link, which may point outside the layer.
func TestLookupLayerDoesNotFollowSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "src/main.py"), "outside\n")
	layer := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(layer, "app")); err != nil {
		t.Fatal(err)
	}

	got, hostPath, err := lookupLayer(layer, "/app/src/main.py")
	if err != nil {
		t.Fatal(err)
	}
	if got != layerHidden || hostPath != "" {
		t.Fatalf("lookupLayer = %d %q, want hidden and no path", got, hostPath)
	}
}

func TestLookupLayerWhiteoutsAndOpaqueDirectories(t *testing.T) {
	const target = "/app/src/main.py"
	cases := []struct {
		name  string
		build func(t *testing.T, layer string)
		want  layerEntry
	}{
		{"whiteout at the target", func(t *testing.T, layer string) {
			writeTestCharacterDeviceWhiteout(t, filepath.Join(layer, "app/src/main.py"))
		}, layerHidden},
		{"whiteout above the target", func(t *testing.T, layer string) {
			writeTestCharacterDeviceWhiteout(t, filepath.Join(layer, "app/src"))
		}, layerHidden},
		{"xattr whiteout at the target", func(t *testing.T, layer string) {
			writeTestXattrWhiteout(t, filepath.Join(layer, "app/src/main.py"))
		}, layerHidden},
		{"opaque directory without the target", func(t *testing.T, layer string) {
			writeTestOpaqueDirectory(t, filepath.Join(layer, "app"))
		}, layerHidden},
		{"opaque directory two levels above the missing entry", func(t *testing.T, layer string) {
			writeTestOpaqueDirectory(t, filepath.Join(layer, "app"))
			writeTestFile(t, filepath.Join(layer, "app/src/other.py"), "x\n")
		}, layerHidden},
		{"opaque directory with the target", func(t *testing.T, layer string) {
			writeTestOpaqueDirectory(t, filepath.Join(layer, "app"))
			writeTestFile(t, filepath.Join(layer, "app/src/main.py"), "x\n")
		}, layerPresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layer := t.TempDir()
			tc.build(t, layer)
			got, _, err := lookupLayer(layer, target)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("lookupLayer = %d, want %d", got, tc.want)
			}
		})
	}
}

func writeTestXattrWhiteout(t *testing.T, path string) {
	t.Helper()
	writeTestFile(t, path, "")
	if err := unix.Setxattr(path, "trusted.overlay.whiteout", []byte("y"), 0); err != nil {
		t.Skipf("cannot set trusted.overlay.whiteout in this environment: %v", err)
	}
}

// eventsOfVersions writes the given contents as successive versions of one
// file and returns the events of a blame over them.
func eventsOfVersions(t *testing.T, contents []string) []*blameEvent {
	t.Helper()
	base := t.TempDir()
	st := &state{ckptID: "original"}
	var events []*blameEvent
	for i, content := range contents {
		layer := filepath.Join(base, fmt.Sprintf("layer%d", i))
		writeTestFile(t, filepath.Join(layer, "app/file.txt"), content)
		event, err := advance(st, layer, fmt.Sprintf("c%d", i), "/app/file.txt")
		if err != nil || event == nil {
			t.Fatalf("advance over layer %d: event %v, error %v", i, event, err)
		}
		events = append(events, event)
	}
	return events
}

func TestBuildBlameRecordsKeepsLayerOrder(t *testing.T) {
	var contents []string
	for i := 0; i < 40; i++ {
		content := strings.Repeat(fmt.Sprintf("line of version %d\n", i/2), 1+(i/2)%7)
		contents = append(contents, content) // every other version repeats the one before
	}
	events := eventsOfVersions(t, contents)

	var want []BlameCkptRecord
	for _, event := range events {
		record, err := buildBlameRecord(event, newCompareBufs(), newDiffBudget())
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, record)
	}
	metadata := 0
	for _, record := range want {
		if record.ChangeType == ChangeTypeMetadata {
			metadata++
		}
	}
	if want[0].ChangeType != ChangeTypeAdded || metadata == 0 || metadata == len(want)-1 {
		t.Fatalf("test versions give %d metadata records of %d, want a mix after an added one", metadata, len(want))
	}

	saved := compareWorkers
	defer func() { compareWorkers = saved }()
	for _, workers := range []int{1, 3, 16} {
		compareWorkers = workers
		got, err := buildBlameRecords(events)
		if err != nil {
			t.Fatalf("%d workers: %v", workers, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%d workers: records differ from the ones built one by one", workers)
		}
	}
}

func TestBuildBlameRecordsWithoutEventsIsEmpty(t *testing.T) {
	records, err := buildBlameRecords(nil)
	if err != nil || records == nil || len(records) != 0 {
		t.Fatalf("buildBlameRecords(nil) = %v, %v, want an empty non-nil list", records, err)
	}
}

func TestBuildBlameRecordsReturnsAnError(t *testing.T) {
	events := eventsOfVersions(t, []string{"a\n", "b\n", "c\n", "d\n", "e\n", "f\n"})
	if err := os.Remove(events[3].next.rootViewPath); err != nil {
		t.Fatal(err)
	}
	saved := compareWorkers
	defer func() { compareWorkers = saved }()
	for _, workers := range []int{1, 4} {
		compareWorkers = workers
		if _, err := buildBlameRecords(events); err == nil || !strings.Contains(err.Error(), "c3") {
			t.Fatalf("%d workers: error = %v, want one that names checkpoint c3", workers, err)
		}
	}
}

func TestReadFileStringMatchesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	content := strings.Repeat("0123456789\n", 20000)
	writeTestFile(t, path, content)
	for _, size := range []int64{0, 10, int64(len(content))} { // the size is only a hint
		got, err := readFileString(path, size)
		if err != nil {
			t.Fatal(err)
		}
		if got != content {
			t.Fatalf("readFileString with size hint %d returned %d bytes, want %d", size, len(got), len(content))
		}
	}
}

// Diffs run together while their inputs fit the budget; one that does not fit
// waits, and one larger than the whole budget runs alone.
func TestDiffBudgetBoundsTheBytesInFlight(t *testing.T) {
	budget := newDiffBudget()
	waits := func(size int64) (acquired chan func(), waited bool) {
		acquired = make(chan func(), 1)
		go func() { acquired <- budget.acquire(size) }()
		select {
		case release := <-acquired:
			acquired <- release
			return acquired, false
		case <-time.After(50 * time.Millisecond):
			return acquired, true
		}
	}

	half := int64(blameDiffBudgetBytes / 2)
	first, waited := waits(half)
	if waited {
		t.Fatal("the first diff waited")
	}
	second, waited := waits(half)
	if waited {
		t.Fatal("a diff that fits the budget waited")
	}
	third, waited := waits(1)
	if !waited {
		t.Fatal("a diff entered over the budget")
	}
	(<-first)()
	(<-third)() // one half and one byte now fit
	(<-second)()

	huge, waited := waits(3 * blameDiffBudgetBytes)
	if waited {
		t.Fatal("a diff larger than the budget waited although nothing was running")
	}
	small, waited := waits(1)
	if !waited {
		t.Fatal("a diff entered beside one larger than the budget")
	}
	(<-huge)()
	(<-small)()
}
