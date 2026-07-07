package waypoint

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
func NewManagerWithSession() (*Manager, string, error) {
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate session ID: %w", err)
	}

	loadConfig()

	baseDir := filepath.Join(DefaultSessionsDir, sessionID)
	manager := NewManager(baseDir)
	manager.sessionID = sessionID
	manager.shellPid = ShellNotEnabled
	manager.shellSocket = ""

	// Save session info globally
	if err := saveSessionInfo(sessionID, manager); err != nil {
		return nil, "", fmt.Errorf("failed to save session info: %w", err)
	}

	return manager, sessionID, nil
}

// LoadManager loads an existing manager by session ID
func LoadManager(sessionID string) (*Manager, error) {
	sessionInfo, err := loadSessionInfo(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session: %w", err)
	}

	manager := NewManager(sessionInfo.BaseDir)
	manager.sessionID = sessionID
	manager.originalDir = sessionInfo.OriginalDir
	manager.workOverlay = sessionInfo.WorkOverlay
	manager.shellPid = sessionInfo.ShellPid
	manager.shellSocket = sessionInfo.ShellSocket

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
func saveSessionInfo(sessionID string, manager *Manager) error {
	os.MkdirAll(SessionInfoDir, 0755)

	sessionInfo := SessionInfo{
		SessionID:   sessionID,
		BaseDir:     manager.baseDir,
		OriginalDir: manager.originalDir,
		WorkOverlay: manager.workOverlay,
		CreatedAt:   time.Now().Unix(),
		ShellPid:    manager.shellPid,
		ShellSocket: manager.shellSocket,
	}

	data, err := json.MarshalIndent(sessionInfo, "", "  ")
	if err != nil {
		return err
	}

	sessionFile := filepath.Join(SessionInfoDir, sessionID+".json")
	return atomicWriteFile(sessionFile, data, 0o644)
}

// Load SessionInfo from the fixed-path global store
func loadSessionInfo(sessionID string) (*SessionInfo, error) {
	sessionFile := filepath.Join(SessionInfoDir, sessionID+".json")

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	var sessionInfo SessionInfo
	err = json.Unmarshal(data, &sessionInfo)
	return &sessionInfo, err
}

// Remove SessionInfo JSON file from the fixed-path global store
func removeSessionInfo(sessionID string) error {
	sessionFile := filepath.Join(SessionInfoDir, sessionID+".json")
	return os.Remove(sessionFile)
}
