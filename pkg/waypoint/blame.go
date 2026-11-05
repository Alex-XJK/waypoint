package waypoint

// TODO add description

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type BlameResult struct {
	TargetPath string `json:"target_path"`
	TargetCkptID string `json:"target_checkpoint_id"`
	// TODO should we output all ckptIDs in the ancestry, with empty blame if necessary?
	Records []BlameCkptRecord `json:"blamed_checkpoints"`
}

type BlameCkptRecord struct {
	CheckpointID string     `json:"checkpoint_id"`
	ChangeType   ChangeType `json:"change_type"`
	// TODO: add field containing the actual changes from previous
}

func isOpaque(dir string) (bool, error) {
	value := make([]byte, 10)
	n, err := unix.Getxattr(dir, "trusted.overlay.opaque", value)
	if err != nil {
		if errors.Is(err, unix.ENODATA) {
			return false, nil // Xattr doesn't exist; not opaque.
		}
		return false, fmt.Errorf("get opaque xattr for %s: %w", dir, err)
	}

	return string(value[:n]) == "y", nil
}

func isWhiteout(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	// Whiteout represented as a character device with major/minor 0/0.
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if info.Mode()&os.ModeCharDevice != 0 &&
			int(unix.Major(uint64(stat.Rdev))) == 0 &&
			int(unix.Minor(uint64(stat.Rdev))) == 0 {
			return true, nil
		}
	}
	// Whiteout represented as a zero-size regular file with xattr.
	if info.Mode().IsRegular() && info.Size() == 0 {
		_, err := unix.Getxattr(path, "trusted.overlay.whiteout", nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func isDescendant(dirPath string, targetPath string) (bool, error) {
	childPath, err := filepath.Rel(dirPath, targetPath)
	if err != nil {
		return false, err
	}
	if childPath == "." || childPath == ".." || strings.HasPrefix(childPath, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

func checkDir(rootViewDir, containerViewDir, targetPath string, visible *bool, blamed *[]BlameCkptRecord, ckptID string) (bool, error) {
	dentries, err := os.ReadDir(rootViewDir)
	if err != nil {
		return false, fmt.Errorf("read directory %s: %w", rootViewDir, err)
	}
	for _, dentry := range dentries {
		rootViewPath := filepath.Join(rootViewDir, dentry.Name())
		containerViewPath := filepath.Join(containerViewDir, dentry.Name())
		if !dentry.IsDir() { //TODO won't follow symlinks, change?
			whiteout, err := isWhiteout(rootViewPath)
			if err != nil {
				return false, err
			}
			if whiteout {
				if *visible {
					if containerViewPath == targetPath { // whiteout of target file
						*visible = false
						if blamed != nil {
							*blamed = append(*blamed, BlameCkptRecord{
								CheckpointID: ckptID,
								ChangeType:   ChangeTypeDeleted,
							})
						}
						return true, nil
					} else { // whiteout could represent directory, if so check if target is child
						hide, err := isDescendant(containerViewPath, targetPath)
						if err != nil {
							return false, err
						}
						if hide {
							*visible = false
							if blamed != nil {
								*blamed = append(*blamed, BlameCkptRecord{
									CheckpointID: ckptID,
									ChangeType:   ChangeTypeDeleted,
								})
							}
							return true, nil
						}
					}
				}
			} else if containerViewPath == targetPath {
				if blamed != nil {
					if !*visible {
						*blamed = append(*blamed, BlameCkptRecord{
							CheckpointID: ckptID,
							ChangeType:   ChangeTypeAdded,
						})
					} else {
						*blamed = append(*blamed, BlameCkptRecord{
							CheckpointID: ckptID,
							ChangeType:   ChangeTypeModified,
						})
					}
				}
				*visible = true
				return true, nil
			}
		} else {
			updated, err := checkDir(rootViewPath, containerViewPath, targetPath, visible, blamed, ckptID)
			if err != nil {
				return false, err
			}
			if updated {
				return true, nil
			}
			opaque, err := isOpaque(rootViewPath)
			if err != nil {
				return false, err
			}
			if opaque && *visible {
				hide, err := isDescendant(containerViewPath, targetPath)
				if err != nil {
					return false, err
				}
				if hide {
					*visible = false
					if blamed != nil {
						*blamed = append(*blamed, BlameCkptRecord{
							CheckpointID: ckptID,
							ChangeType:   ChangeTypeDeleted,
						})
					}
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// very similar to diff.go normalizeTargetDir, refactor?
func normalizePath(targetDir string) (string, error) {
	targetDir = strings.TrimLeft(targetDir, "/")
	clean := filepath.Clean(targetDir)

	if clean == "." {
		return "", fmt.Errorf("path is for root directory, blame does not support directory targets") // only diff to normalizeTargetDir
	}
	if clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must not escape the filesystem root")
	}
	return "/" + clean, nil
}

// Entry point for blame
func (m *Manager) PrintBlame(ckptID, path string) error {
	if err := validateCheckpointID(ckptID); err != nil {
		return fmt.Errorf("invalid checkpoint ID %s: %w", ckptID, err)
	}
	metadata, err := m.loadMetadata(ckptID)
	if err != nil {
		return fmt.Errorf("failed to read metadata for checkpoint %s: %w", ckptID, err)
	}
	if metadata.Status != CheckpointStatusReady {
		return fmt.Errorf("checkpoint %s is not ready (status: %s)", ckptID, metadata.Status)
	}
	normalizedTargetPath, err := normalizePath(path)
	if err != nil {
		return err
	}

	blamedCkpts := make([]BlameCkptRecord, 0)

	ckptIDs := append([]string(nil), metadata.LayerIDs...)
	originalDir := metadata.OriginalDir
	// first check through original dir to set up visibility of target path
	visible := false
	_, err = checkDir(originalDir, "/", normalizedTargetPath, &visible, nil, "")
	if err != nil {
		return err
	}
	// then advance through checkpoint layers, blaming
	for _, ckptID := range ckptIDs {
		upperDirPath := m.checkpointUpperDir(ckptID)
		_, err := checkDir(upperDirPath, "/", normalizedTargetPath, &visible, &blamedCkpts, ckptID)
		if err != nil {
			return err
		}
	}

	result := BlameResult{
		TargetPath: normalizedTargetPath,
		TargetCkptID: ckptID,
		Records:    blamedCkpts,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encode JSON to stdout: %w", err)
	}

	return nil
}
