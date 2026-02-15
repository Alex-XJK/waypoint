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

// NewManagerWithSession creates a new manager with a random session ID
// Uses default branch ID for new session
func NewManagerWithSession() (*Manager, string, string, error) {
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	loadConfig()

	baseDir := filepath.Join(DefaultSessionsDir, sessionID)
	manager := NewManager(baseDir)
	manager.sessionID = sessionID
	manager.branchID = DefaultBranchID
	manager.currDir = filepath.Join(baseDir, manager.branchID)
	manager.currentParent = []string{}
	manager.shellPid = ShellNotEnabled
	manager.shellSocket = ""

	// Save session info globally
	if err := saveSessionInfo(sessionID, branchID, manager); err != nil { // why pass in sessionID? need to pass in branchID?
		return nil, "", fmt.Errorf("failed to save session info: %w", err)
	}

	return manager, sessionID, DefaultBranchID, nil
}

// LoadManager loads an existing manager by session ID and branch ID
func LoadManager(sessionID string, branchID string) (*Manager, error) {
	sessionInfo, err := loadSessionInfo(sessionID, branchID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}

	manager := NewManager(sessionInfo.BaseDir)
	manager.sessionID = sessionID
	manager.branchID = sessionInfo.BranchID
	manager.currDir = sessionInfo.CurrDir
	manager.originalDir = sessionInfo.OriginalDir
	manager.workOverlay = sessionInfo.WorkOverlay
	manager.shellPid = sessionInfo.ShellPid
	manager.shellSocket = sessionInfo.ShellSocket
	manager.currentParent = sessionInfo.CurrentParent

	return manager, nil
}

// Generate a random session ID
func generateSessionID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// Convert Manager to SessionInfo and save to the fixed-path global store
func saveSessionInfo(sessionID string, branchID string, manager *Manager) error {
	os.MkdirAll(SessionInfoDir, 0755)

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

// Remove all SessionInfo JSON files for a given session ID from the fixed-path global store
// add way to remove just a specific branch's session info?
func removeSessionInfo(sessionID string) error {
	sessionIDDir := filepath.Join(SessionInfoDir, sessionID)
	return os.RemoveAll(sessionIDDir)
}
