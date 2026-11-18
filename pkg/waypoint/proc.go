package waypoint

// TODO add description
/*
import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_fs "github.com/checkpoint-restore/go-criu/v8/crit/images/fs"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	criu_fdinfo "github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"golang.org/x/sys/unix"
)

/* andy remaining
type ProcessRecord struct {
	Key      string     `json:"key"`

	Internal bool       `json:"internal,omitempty"`

	Exe      string     `json:"exe,omitempty"`
	Argv     []string   `json:"argv"`
	Env      []string   `json:"env"` // initial environment, sorted
	
	Threads  int        `json:"threads"`
	FDs      []FDRecord `json:"fds"`
}
*/
/*
type ProcInfo struct {
	PID 	uint32 `json:"pid"`
	PGID 	uint32 	`json:"pgid"`
	PPID 	uint32	`json:"ppid"`
	SID		uint32	`json:"sid"`
	Comm	string	`json:"comm"`
	State	string	`json:"state"`
	UID      uint32     `json:"uid"`
	GID      uint32     `json:"gid"`
	EUID     uint32     `json:"euid"`
	EGID     uint32     `json:"egid"`
	Cwd      string  `json:"cwd,omitempty"`
	Root     string   `json:"root,omitempty"`
}

type fileInfo struct {
    Kind   string
    Target string
    Access string
}

type fileTable struct {
    byID map[uint32]fileInfo
}

func (t *fileTable) get(id uint32) fileInfo {
    if info, ok := t.byID[id]; ok {
        return info
    }

    return fileInfo{Kind: "unknown"}
}

func getCriuDataForPID(criuImgDir string, pid uint32) (*criu_core.CoreEntry, *criu_fs.FsEntry, error) {
	coreFile, err := os.Open(filepath.Join(criuImgDir, fmt.Sprintf("core-%d.img", pid)))
	if err != nil {
		return nil, nil, err
	}
	coreCrit := crit.New(coreFile, nil, "", false, false)
	coreImg, err := coreCrit.Decode(&criu_core.CoreEntry{})
	coreFile.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(coreImg.Entries) == 0 {
		return nil, nil, fmt.Errorf("no core entries for PID %d", pid)
	}
	coreData, ok := coreImg.Entries[0].Message.(*criu_core.CoreEntry)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected core entry type for PID %d", pid)
	}

	fsFile, err := os.Open(filepath.Join(criuImgDir, fmt.Sprintf("fs-%d.img", pid)))
	if err != nil {
		return nil, nil, err
	}
	fsCrit := crit.New(fsFile, nil, "", false, false)
	fsImg, err := fsCrit.Decode(&criu_fs.FsEntry{})
	fsFile.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(fsImg.Entries) == 0 {
		return nil, nil, fmt.Errorf("no fs entries for PID %d", pid)
	}
	fsData, ok := fsImg.Entries[0].Message.(*criu_fs.FsEntry)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected fs entry type for PID %d", pid)
	}
	return coreData, fsData, nil
}

func accessMode(flags uint32) string {
    var mode string

    switch flags & 3 {
    case 0:
        mode = "r"
    case 1:
        mode = "w"
    default:
        mode = "rw"
    }

    if flags&0o2000 != 0 {
        mode += "a"
    }

    return mode
}

func processFilesImg(criuImgDir string) (*fileTable, error) {
    filesPath := filepath.Join(criuImgDir, "files.img")

    filesFile, err := os.Open(filesPath)
    if err != nil {
        return nil, fmt.Errorf("open files.img: %w", err)
    }

    filesCrit := crit.New(filesFile, nil, "", false, false)
    filesImg, err := filesCrit.Decode(&criu_fdinfo.FileEntry{})
    filesFile.Close()
    if err != nil {
        return nil, fmt.Errorf("decode files.img: %w", err)
    }

    table := &fileTable{
        byID: make(map[uint32]fileInfo),
    }

    for _, entry := range filesImg.Entries {
        file, ok := entry.Message.(*criu_fdinfo.FileEntry)
        if !ok {
            return nil, fmt.Errorf("unexpected files.img entry type")
        }

        id := file.GetId()
        kind := file.GetType().String()

        info := fileInfo{
            Kind: kind,
        }

        switch file.GetType() {
        case criu_fdinfo.FdTypes_REG:
            reg := file.GetReg()

            if reg != nil {
                info.Target = reg.GetName()
                info.Access = accessMode(reg.GetFlags())
            }
        }

        table.byID[id] = info
    }

    return table, nil
}

func populateProcInfos(criuImgDir string, map[uint32]ProcInfo) error {
	// to do better handle errors so don't crash?
	psTreeFile, err := os.Open(filepath.Join(criuImgDir, "pstree.img"))
	if err != nil {
		return err
	}
	psTreeCrit := crit.New(psTreeFile, nil, "", false, false)
	psTreeImg, err := psTreeCrit.Decode(&pstree.PstreeEntry{})
	psTreeFile.Close()
	if err != nil {
		return err
	}
	filesIDtoInfo, err := processFilesImg(criuImgDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range psTreeImg.Entries {
		process, ok := entry.Message.(*pstree.PstreeEntry)
		if !ok {
			return nil, fmt.Errorf("unexpected pstree entry type")
		}
		pid := process.GetPid()
		coreData, fsData, err := getCriuDataForPID(criuImgDir, pid)
		if err != nil {
			return nil, fmt.Errorf("get CRIU data for PID %d: %w", pid, err) // TODO instead mark incomplete and keep going?
		}
		tc := coreData.GetTc()
		cred := tc.GetCreds()

		cwdID := fsData.GetCwdId()
		rootID := fsData.GetRootId()
		cwdInfo := filesIDtoInfo.get(cwdID)
		rootInfo := filesIDtoInfo.get(rootID)
		cwd := cwdInfo.Target
		root := rootInfo.Target

		processes[pid] = ProcInfo{
			PID:  pid,
			PPID: process.GetPpid(),
			PGID: process.GetPgid(),
			SID:  process.GetSid(),

			Comm: tc.GetComm(),
			State: tc.GetTaskState(),

			UID:  cred.GetUid(),
			GID:  cred.GetGid(),
			EUID: cred.GetEuid(),
			EGID: cred.GetEgid(),

			Cwd:  cwd,
			Root: root,
		}
	}
}

func (m *Manager) getProcDiff(ckptIDLeft, ckptIDRight string, detected *[]DiffProcRecord) error {
	criuDirLeft := m.checkpointCriuDir(ckptIDLeft)
	criuDirRight := m.checkpointCriuDir(ckptIDRight)
	// Hold the images lock shared, as a restore does, so a tmpfs flush
	// cannot repoint the images while they are read.
	if unlock, err := lockImages(criuDirLeft, false); err == nil {
		defer unlock()
	}
	if unlock, err := lockImages(criuDirRight, false); err == nil {
		defer unlock()
	}
	// get all proc on left and right first
	processesLeft := make(map[uint32]ProcInfo)
	processesRight := make(map[uint32]ProcInfo)
	err := populateProcInfos(criuDirLeft, processesLeft)
	err := populateProcInfos(criuDirRight, processesRight)



	expLeft, err := explorePsFlat(criuDirLeft)
	if err != nil {
		return err
	}
	expRight, err := explorePsFlat(criuDirRight)
	if err != nil {
		return err
	}

	for pidLeft, infoLeft := range expLeft {
		infoRight, exists := expRight[pidLeft]
		if !exists {
			*detected = append(*detected, DiffProcRecord{
				PID:        pidLeft,
				ChangeType: ChangeTypeDeleted,
				Process:    &infoLeft,
			})
			continue
		}
		if err := processProcMod(&infoLeft, &infoRight, detected); err != nil {
			return err
		}
	}
	for pid, info := range expRight {
		if _, exists := expLeft[pid]; exists {
			continue
		}
		*detected = append(*detected, DiffProcRecord{
			PID:        pid,
			ChangeType: ChangeTypeAdded,
			Process:    &info,
		})
	}
	return nil
}
*/