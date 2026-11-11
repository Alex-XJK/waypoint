package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

const diffUsage = "Usage: diff <session> <checkpoint-id> [<session>:]<checkpoint-id> [--fs-only | --proc-only] [--content | --word-diff] [--content-max=<bytes>]"
const stateUsage = "Usage: state <session> <checkpoint-id>"

// runDiff prints what differs between two checkpoints. The second may name
// another session as <session>:<checkpoint-id>. --content adds a unified line
// diff to each changed text file of at most --content-max bytes; --word-diff
// does the same with the changed words marked inline.
func runDiff(args []string) error {
	var pos []string
	opts := waypoint.DiffOptions{Filesystem: true, Processes: true}
	only := ""
	content := false
	contentMax := int64(waypoint.DefaultPatchMaxBytes)
	contentMaxSet := false
	for _, a := range args {
		switch {
		case a == "--fs-only" || a == "--proc-only":
			if only != "" && only != a {
				return fmt.Errorf("--fs-only and --proc-only exclude each other")
			}
			only = a
		case a == "--content":
			content = true
		case a == "--word-diff":
			content = true
			opts.PatchWords = true
		case strings.HasPrefix(a, "--content-max="):
			n, err := strconv.ParseInt(strings.TrimPrefix(a, "--content-max="), 10, 64)
			if err != nil || n <= 0 {
				return fmt.Errorf("--content-max needs a positive number of bytes, got %q", strings.TrimPrefix(a, "--content-max="))
			}
			contentMax, contentMaxSet = n, true
		default:
			if strings.HasPrefix(a, "--") {
				return fmt.Errorf("unknown flag: %s\n%s", a, diffUsage)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 3 {
		return fmt.Errorf("%s", diffUsage)
	}
	switch only {
	case "--fs-only":
		opts.Processes = false
	case "--proc-only":
		opts.Filesystem = false
	}
	if contentMaxSet && !content {
		return fmt.Errorf("--content-max needs --content or --word-diff")
	}
	if content {
		if !opts.Filesystem {
			return fmt.Errorf("--content and --word-diff need the filesystem diff; drop --proc-only")
		}
		opts.PatchMaxBytes = contentMax
	}

	from, err := waypoint.LoadManager(pos[0])
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	to, toID := from, pos[2]
	if session, ckpt, ok := strings.Cut(pos[2], ":"); ok {
		if to, err = waypoint.LoadManager(session); err != nil {
			return fmt.Errorf("loading session %s: %w", session, err)
		}
		toID = ckpt
	}
	d, err := waypoint.DiffCheckpoints(from, pos[1], to, toID, opts)
	if err != nil {
		return err
	}
	return printJSON(d)
}

// runState prints the process state of a checkpoint.
func runState(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("%s", stateUsage)
	}
	m, err := waypoint.LoadManager(args[0])
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	st, err := m.CheckpointProcessState(args[1])
	if err != nil {
		return err
	}
	return printJSON(st)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	// Keep <, > and & as they are; patches are full of them.
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
