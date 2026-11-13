package waypoint

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// Share only Harbor's FROM-only contexts. A published rootfs is immutable;
// session writes go to their private overlay, and cleanup leaves the cache alone.
// Key by the actual image ID so rebuilding the same Harbor tag cannot reuse an
// old rootfs. Ordinary Dockerfiles retain private rootfs staging.
var harborBaseImage = regexp.MustCompile(`^FROM localhost/harbor-waypoint:[a-f0-9]{16}\n?$`)
var imageIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func sharedHarborRootfs(context string, quiet bool) (string, ImageConfig, error) {
	var cfg ImageConfig
	cache := os.Getenv("WAYPOINT_ROOTFS_CACHE_DIR")
	if cache == "" {
		return "", cfg, nil
	}
	dockerfile, err := os.ReadFile(filepath.Join(context, "Dockerfile"))
	if err != nil || !harborBaseImage.Match(dockerfile) {
		return "", cfg, err
	}
	ref := strings.TrimSpace(strings.TrimPrefix(string(dockerfile), "FROM "))
	out, err := exec.Command("buildah", "inspect", "--type", "image", "--format", "{{.FromImageID}}", ref).Output()
	if err != nil {
		return "", cfg, fmt.Errorf("inspect cached Harbor image %s: %w", ref, err)
	}
	imageID := strings.TrimPrefix(strings.TrimSpace(string(out)), "sha256:")
	if !imageIDPattern.MatchString(imageID) {
		return "", cfg, fmt.Errorf("invalid Harbor image ID: %q", imageID)
	}
	entry := filepath.Join(cache, imageID)
	if err := os.MkdirAll(cache, 0755); err != nil {
		return "", cfg, err
	}
	lock, err := os.OpenFile(entry+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", cfg, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return "", cfg, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	manifest, err := os.ReadFile(filepath.Join(entry, "config.json"))
	if err == nil {
		if err := json.Unmarshal(manifest, &cfg); err != nil {
			return "", cfg, fmt.Errorf("rootfs cache config: %w", err)
		}
		if info, err := os.Stat(filepath.Join(entry, "rootfs")); err != nil || !info.IsDir() {
			return "", cfg, fmt.Errorf("rootfs cache entry is incomplete: %s", entry)
		}
		return filepath.Join(entry, "rootfs"), cfg, nil
	}
	if !os.IsNotExist(err) {
		return "", cfg, err
	}
	staging, err := os.MkdirTemp(cache, ".building-")
	if err != nil {
		return "", cfg, err
	}
	defer os.RemoveAll(staging)
	// Pin the inspected image even if another process subsequently rebuilds its tag.
	pinnedContext := filepath.Join(staging, "image-"+imageID)
	if err := os.Mkdir(pinnedContext, 0700); err != nil {
		return "", cfg, err
	}
	if err := os.WriteFile(filepath.Join(pinnedContext, "Dockerfile"), []byte("FROM "+imageID+"\n"), 0600); err != nil {
		return "", cfg, err
	}
	if err := BuildFromDockerfile(pinnedContext, filepath.Join(staging, "rootfs"), quiet, &cfg); err != nil {
		return "", cfg, err
	}
	manifest, err = json.Marshal(cfg)
	if err != nil {
		return "", cfg, err
	}
	if err := os.WriteFile(filepath.Join(staging, "config.json"), manifest, 0644); err != nil {
		return "", cfg, err
	}
	// Published entries are never modified or removed by session cleanup. Each
	// session mounts this rootfs below its own writable OverlayFS layers.
	if err := os.Rename(staging, entry); err != nil {
		return "", cfg, err
	}
	return filepath.Join(entry, "rootfs"), cfg, nil
}
