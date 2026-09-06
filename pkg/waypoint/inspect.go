package waypoint

import (
	"fmt"
	"os"
	"sort"
)

// ForkRuntimeInfo describes the host-visible runtime of one fork. HostRoot is
// present only while the recorded process identity is still live and matches.
// This derived view is not persisted; a restored fork gets a new identity.
type ForkRuntimeInfo struct {
	ID                string     `json:"id"`
	BaseCheckpointID  string     `json:"base_checkpoint_id,omitempty"`
	Status            ForkStatus `json:"status"`
	Available         bool       `json:"available"`
	HostPID           int        `json:"host_pid,omitempty"`
	HostPIDStartTime  uint64     `json:"host_pid_start_time,omitempty"`
	HostRoot          string     `json:"host_root,omitempty"`
	Volatile          bool       `json:"volatile"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
}

// InspectForks returns host-visible runtime information for every fork in the
// session. Each record is loaded and validated under that fork's lock so a
// snapshot or destroy cannot change its process identity during inspection.
func (m *Manager) InspectForks() ([]ForkRuntimeInfo, error) {
	entries, err := os.ReadDir(m.forksDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []ForkRuntimeInfo{}, nil
		}
		return nil, err
	}

	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateForkID(entry.Name()) != nil {
			continue
		}
		ids = append(ids, entry.Name())
	}
	sort.Strings(ids)

	infos := make([]ForkRuntimeInfo, 0, len(ids))
	for _, forkID := range ids {
		var info ForkRuntimeInfo
		var loadErr error
		if err := m.withForkLock(forkID, func() error {
			var f *Fork
			f, loadErr = m.loadFork(forkID)
			if loadErr == nil {
				info = inspectForkRuntime(f)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if loadErr != nil {
			continue // fork was removed, or its record is not readable
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func inspectForkRuntime(f *Fork) ForkRuntimeInfo {
	info := ForkRuntimeInfo{
		ID:               f.ID,
		BaseCheckpointID: f.BaseCheckpointID,
		Status:           f.Status,
		HostPID:          f.PID,
		HostPIDStartTime: f.StartTime,
		Volatile:         true,
	}

	if f.Status != ForkStatusRunning {
		info.UnavailableReason = "fork is not running"
		return info
	}
	if f.PID <= 0 {
		info.UnavailableReason = "fork has no host PID"
		return info
	}
	if f.StartTime == 0 {
		info.UnavailableReason = "fork has no recorded PID start time"
		return info
	}
	startTime, err := procStartTime(f.PID)
	if err != nil {
		info.UnavailableReason = "host process is unavailable"
		return info
	}
	if startTime != f.StartTime {
		info.UnavailableReason = "host PID start time does not match"
		return info
	}

	hostRoot := fmt.Sprintf("/proc/%d/root", f.PID)
	root, err := os.OpenRoot(hostRoot)
	if err != nil {
		info.UnavailableReason = "host root is unavailable"
		return info
	}
	_ = root.Close()
	startTime, err = procStartTime(f.PID)
	if err != nil || startTime != f.StartTime {
		info.UnavailableReason = "host process changed while inspecting root"
		return info
	}

	info.Available = true
	info.HostRoot = hostRoot
	return info
}
