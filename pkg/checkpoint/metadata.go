package checkpoint

// Metadata serialization/deserialization

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func (m *Manager) saveMetadata(checkpointID string, metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}

	metadataPath := filepath.Join(m.metadataDir, checkpointID+".json")
	return os.WriteFile(metadataPath, data, 0644)
}

func (m *Manager) loadMetadata(checkpointID string) (*Metadata, error) {
	metadataPath := filepath.Join(m.metadataDir, checkpointID+".json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}

	var metadata Metadata
	err = json.Unmarshal(data, &metadata)
	return &metadata, err
}

// syncManagerToSession updates the session info file with the current manager state
func (m *Manager) syncManagerToSession() error {
	err := saveSessionInfo(m.sessionID, m.branchID, m)
	if err != nil {
		return err
	}
	return nil
}

// since only called on init, could use DefaultBranchID rather than passing in?
func updateSessionEnvironment(sessionID, branchID, originalDir, workOverlay string) error {
	sessionInfo, err := loadSessionInfo(sessionID, branchID)
	if err != nil {
		return err
	}

	sessionInfo.OriginalDir = originalDir
	sessionInfo.WorkOverlay = workOverlay

	data, err := json.MarshalIndent(sessionInfo, "", "  ")
	if err != nil {
		return err
	}

	sessionFile := filepath.Join(SessionInfoDir, sessionID, branchID+".json")
	return os.WriteFile(sessionFile, data, 0644)
}
