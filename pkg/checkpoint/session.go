package checkpoint

// Session management functions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// New ManagerWithBranch creates a new manager with a random branch ID for an existing session
func NewManagerWithBranch(sessionID string) (*Manager, string, string, error) {
	branchID, err := generateID()
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to generate branch ID: %w", err)
	}

	manager := NewManager(sessionID, branchID) 	// original dir NOT set: will be set on RestoreNewBranch

	if err := saveSessionInfo(sessionID, branchID, manager); err != nil {
		return nil, "", "", fmt.Errorf("failed to save session info: %w", err)
	}

	return manager, branchID, manager.workOverlay, nil
}

// NewManagerWithSession creates a new manager with a random session ID
// Uses default branch ID for new session
func NewManagerWithSession() (*Manager, string, string, error) {
	sessionID, err := generateID()
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	loadConfig()

	branchID := DefaultBranchID
	manager := NewManager(sessionID, branchID)

	// Save session info globally
	if err := saveSessionInfo(sessionID, branchID, manager); err != nil { // why pass in sessionID? need to pass in branchID?
		return nil, "", "", fmt.Errorf("failed to save session info: %w", err)
	}

	return manager, sessionID, DefaultBranchID, nil
}

// LoadManager loads an existing manager by session ID and branch ID
func LoadManager(sessionID string, branchID string) (*Manager, error) {
	sessionInfo, err := loadSessionInfo(sessionID, branchID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}

	manager := NewManager(sessionID, branchID)
	manager.shellPid = sessionInfo.ShellPid
	manager.shellSocket = sessionInfo.ShellSocket
	manager.currentParent = sessionInfo.CurrentParent
	manager.originalDir = sessionInfo.OriginalDir
	manager.shellRdev = sessionInfo.ShellRdev
	manager.shellDev = sessionInfo.ShellDev

	return manager, nil
}

// Generate a random ID
func generateID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// Convert Manager to SessionInfo and save to the fixed-path global store
func saveSessionInfo(sessionID string, branchID string, manager *Manager) error {
	os.MkdirAll(filepath.Join(SessionInfoDir, sessionID), 0755)

	sessionInfo := SessionInfo{
		SessionID:     sessionID, // why is sessionID passed in as an arg instead of using manager.sessionID?
		BranchID:      branchID, // not sure if should pass in as arg or just use from manager
		CurrDir:       manager.currDir,
		BaseDir:       manager.baseDir,
		OriginalDir:   manager.originalDir,
		WorkOverlay:   manager.workOverlay,
		CreatedAt:     time.Now().Unix(),
		CurrentParent: manager.currentParent,
		ShellPid:      manager.shellPid,
		ShellSocket:   manager.shellSocket,
		ShellRdev:     manager.shellRdev,
		ShellDev: 	   manager.shellDev,
	}

	data, err := json.MarshalIndent(sessionInfo, "", "  ")
	if err != nil {
		return err
	}

	sessionFile := filepath.Join(SessionInfoDir, sessionID, branchID+".json")
	return os.WriteFile(sessionFile, data, 0644)
}

// Load SessionInfo from the fixed-path global store
func loadSessionInfo(sessionID string, branchID string) (*SessionInfo, error) {
	sessionFile := filepath.Join(SessionInfoDir, sessionID, branchID +".json")

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	var sessionInfo SessionInfo
	err = json.Unmarshal(data, &sessionInfo)
	return &sessionInfo, err
}

func loadSessionInfoJSON(jsonFilePath string) (*SessionInfo, error) {
	data, err := os.ReadFile(jsonFilePath)
	if err != nil {
		return nil, fmt.Errorf("session branch not found: %w", err)
	}
	var sessionInfo SessionInfo
	err = json.Unmarshal(data, &sessionInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal session info: %w", err)
	}
	return &sessionInfo, nil
}

// Remove all SessionInfo JSON files for a given session ID from the fixed-path global store
// add way to remove just a specific branch's session info?
func removeSessionInfo(sessionID string) error {
	sessionIDDir := filepath.Join(SessionInfoDir, sessionID)
	return os.RemoveAll(sessionIDDir)
}
