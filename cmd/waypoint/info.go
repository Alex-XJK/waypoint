package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

type infoOutput struct {
	Version      string              `json:"version"`
	Config       waypoint.ConfigInfo `json:"config"`
	Dependencies dependenciesInfo    `json:"dependencies"`
}

type dependenciesInfo struct {
	CRIU      commandInfo   `json:"criu"`
	Buildah   commandInfo   `json:"buildah"`
	OverlayFS overlayFSInfo `json:"overlayfs"`
}

type commandInfo struct {
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

type overlayFSInfo struct {
	Available     bool   `json:"available"`
	KernelVersion string `json:"kernel_version,omitempty"`
	Source        string `json:"source,omitempty"`
	Error         string `json:"error,omitempty"`
}

func printInfo() error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(collectInfo())
}

func collectInfo() infoOutput {
	return infoOutput{
		Version: Version,
		Config:  waypoint.LoadConfigInfo(),
		Dependencies: dependenciesInfo{
			CRIU:      inspectCommand("criu", "--version"),
			Buildah:   inspectCommand("buildah", "--version"),
			OverlayFS: inspectOverlayFS(),
		},
	}
}

func inspectCommand(name string, versionArgs ...string) commandInfo {
	path, err := exec.LookPath(name)
	if err != nil {
		return commandInfo{
			Available: false,
			Error:     err.Error(),
		}
	}

	info := commandInfo{
		Available: true,
		Path:      path,
	}
	if len(versionArgs) == 0 {
		versionArgs = []string{"--version"}
	}
	out, err := exec.Command(path, versionArgs...).CombinedOutput()
	if err != nil {
		info.Error = commandError(err, out)
		return info
	}
	info.Version = firstLine(strings.TrimSpace(string(out)))
	return info
}

func inspectOverlayFS() overlayFSInfo {
	const procFilesystems = "/proc/filesystems"
	info := overlayFSInfo{Source: procFilesystems}

	data, err := os.ReadFile(procFilesystems)
	if err != nil {
		info.Error = err.Error()
		return info
	}

	info.Available = filesystemListed(data, "overlay")
	if !info.Available {
		info.Error = "overlay not listed in /proc/filesystems"
	}

	out, err := exec.Command("uname", "-r").CombinedOutput()
	if err != nil {
		unameErr := commandError(err, out)
		if info.Error == "" {
			info.Error = unameErr
		} else {
			info.Error += "; uname -r: " + unameErr
		}
		return info
	}
	info.KernelVersion = firstLine(strings.TrimSpace(string(out)))
	return info
}

func filesystemListed(data []byte, name string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == name {
			return true
		}
	}
	return false
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func commandError(err error, output []byte) string {
	message := err.Error()
	if out := firstLine(strings.TrimSpace(string(output))); out != "" {
		message += ": " + out
	}
	return message
}
