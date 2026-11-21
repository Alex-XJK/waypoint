package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

const blameUsage = "Usage: blame <session> <checkpoint-id> <path>... [--lines] [--paths-from FILE|-]"

// blameArgs is the parsed tail of `blame <session> <checkpoint-id> ...`.
type blameArgs struct {
	paths     []string
	lines     bool
	pathsFrom string // file with one path per line, or "-" for stdin
}

// parseBlameArgs parses the arguments after the checkpoint ID. Arguments that
// start with "--" are flags; the others are paths.
func parseBlameArgs(rest []string) (blameArgs, error) {
	var a blameArgs
	for i := 0; i < len(rest); i++ {
		switch arg := rest[i]; {
		case arg == "--lines":
			a.lines = true
		case arg == "--paths-from":
			if i+1 >= len(rest) {
				return a, errors.New("--paths-from requires a value")
			}
			if a.pathsFrom != "" {
				return a, errors.New("--paths-from given twice")
			}
			a.pathsFrom = rest[i+1]
			i++
		case strings.HasPrefix(arg, "--"):
			return a, fmt.Errorf("unknown flag: %s", arg)
		default:
			a.paths = append(a.paths, arg)
		}
	}
	if len(a.paths) == 0 && a.pathsFrom == "" {
		return a, errors.New("no path given")
	}
	return a, nil
}

// readBlamePaths reads one path per line; blank lines are skipped.
func readBlamePaths(r io.Reader) ([]string, error) {
	var paths []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		if line := strings.TrimRight(scanner.Text(), "\r"); strings.TrimSpace(line) != "" {
			paths = append(paths, line)
		}
	}
	return paths, scanner.Err()
}

// runBlame prints one JSON object for a single path given as an argument (the
// original output), and a JSON array, one entry per path in order, otherwise.
func runBlame(sessionID, ckptID string, a blameArgs) error {
	paths := a.paths
	if a.pathsFrom != "" {
		in := os.Stdin
		if a.pathsFrom != "-" {
			f, err := os.Open(a.pathsFrom)
			if err != nil {
				return fmt.Errorf("open paths file: %w", err)
			}
			defer f.Close()
			in = f
		}
		more, err := readBlamePaths(in)
		if err != nil {
			return fmt.Errorf("read paths: %w", err)
		}
		paths = append(paths, more...)
	}
	manager, err := waypoint.LoadManager(sessionID)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	if len(a.paths) == 1 && a.pathsFrom == "" {
		if a.lines {
			return manager.PrintBlameLines(ckptID, paths[0])
		}
		return manager.PrintBlame(ckptID, paths[0])
	}
	out := bufio.NewWriter(os.Stdout)
	if err := manager.PrintBlameMany(ckptID, paths, a.lines, out); err != nil {
		return err
	}
	return out.Flush()
}
