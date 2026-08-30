package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

type systemInfoOutput struct {
	Type         string              `json:"type"`
	Version      string              `json:"version"`
	Config       waypoint.ConfigInfo `json:"config"`
	Dependencies dependenciesInfo    `json:"dependencies"`
}

type sessionInfoOutput struct {
	Type    string                     `json:"type"`
	Session *waypoint.SessionInfo      `json:"session"`
	Forks   []waypoint.ForkRuntimeInfo `json:"forks"`
}

type checkpointInfoOutput struct {
	Type         string             `json:"type"`
	SessionID    string             `json:"session_id"`
	CheckpointID string             `json:"checkpoint_id"`
	Metadata     *waypoint.Metadata `json:"metadata"`
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

func printInfo(args []string) error {
	var output any
	var err error

	switch len(args) {
	case 0:
		output = collectSystemInfo()
	case 1:
		output, err = collectSessionInfo(args[0])
	case 2:
		output, err = collectCheckpointInfo(args[0], args[1])
	}
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func collectSystemInfo() systemInfoOutput {
	return systemInfoOutput{
		Type:    "system",
		Version: Version,
		Config:  waypoint.LoadConfigInfo(),
		Dependencies: dependenciesInfo{
			CRIU:      inspectCommand("criu", "--version"),
			Buildah:   inspectCommand("buildah", "--version"),
			OverlayFS: inspectOverlayFS(),
		},
	}
}

func collectSessionInfo(sessionID string) (sessionInfoOutput, error) {
	sessionInfo, err := waypoint.LoadSessionInfo(sessionID)
	if err != nil {
		return sessionInfoOutput{}, err
	}
	manager, err := waypoint.LoadManager(sessionID)
	if err != nil {
		return sessionInfoOutput{}, err
	}
	forks, err := manager.InspectForks()
	if err != nil {
		return sessionInfoOutput{}, err
	}
	return sessionInfoOutput{
		Type:    "session",
		Session: sessionInfo,
		Forks:   forks,
	}, nil
}

func collectCheckpointInfo(sessionID, checkpointID string) (checkpointInfoOutput, error) {
	metadata, err := waypoint.LoadCheckpointMetadata(sessionID, checkpointID)
	if err != nil {
		return checkpointInfoOutput{}, err
	}
	return checkpointInfoOutput{
		Type:         "checkpoint",
		SessionID:    sessionID,
		CheckpointID: checkpointID,
		Metadata:     metadata,
	}, nil
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
