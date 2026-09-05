package main

import (
	"fmt"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

const copyUsage = "Usage: cp <session> <host-path> <fork-id>:/<path>\n       cp <session> <fork-id>:/<path> <host-path>"

type copyEndpoint struct {
	hostPath  string
	forkID    string
	guestPath string
}

func (e copyEndpoint) isFork() bool {
	return e.forkID != ""
}

func runCopy(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("%s", copyUsage)
	}

	source, err := parseCopyEndpoint(args[1])
	if err != nil {
		return err
	}
	destination, err := parseCopyEndpoint(args[2])
	if err != nil {
		return err
	}
	if source.isFork() == destination.isFork() {
		return fmt.Errorf("copy requires exactly one fork path in <fork-id>:/<path> form")
	}

	manager, err := waypoint.LoadManager(args[0])
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if source.isFork() {
		return manager.CopyFromFork(source.forkID, source.guestPath, destination.hostPath)
	}
	return manager.CopyToFork(destination.forkID, source.hostPath, destination.guestPath)
}

func parseCopyEndpoint(value string) (copyEndpoint, error) {
	if value == "" {
		return copyEndpoint{}, fmt.Errorf("copy path must not be empty")
	}

	colon := strings.IndexByte(value, ':')
	if colon <= 0 || strings.ContainsAny(value[:colon], `/\\`) {
		return copyEndpoint{hostPath: value}, nil
	}

	forkID := value[:colon]
	guestPath := value[colon+1:]
	if guestPath == "" {
		return copyEndpoint{}, fmt.Errorf("fork path %q has an empty path", value)
	}
	// Requiring an absolute guest path keeps the endpoint syntax unambiguous:
	// ordinary host filenames such as image:latest remain host paths.
	if !strings.HasPrefix(guestPath, "/") {
		return copyEndpoint{hostPath: value}, nil
	}
	if !validCopyForkID(forkID) {
		return copyEndpoint{}, fmt.Errorf("fork path %q has an invalid fork ID", value)
	}
	return copyEndpoint{forkID: forkID, guestPath: guestPath}, nil
}

func validCopyForkID(id string) bool {
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return id != ""
}
