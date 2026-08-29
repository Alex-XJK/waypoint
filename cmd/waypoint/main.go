package main

// waypoint: A lightweight process checkpointing and restoration tool
//
// GitHub Repository: https://github.com/Alex-XJK/waypoint.git
// Designed and developed by Alex Jiakai Xu (https://alex-xjk.github.io/), DAPLab @ Columbia University (https://daplab.cs.columbia.edu/)

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

var Version = "v0.7.0"

// printExecResult writes the command output to stdout and exits with the
// command's own exit code, so `waypoint exec` composes like ssh/docker exec.
func printExecResult(result *waypoint.ExecResult) {
	fmt.Print(result.Output)
	if result.Output != "" && !strings.HasSuffix(result.Output, "\n") {
		fmt.Println()
	}
	if result.TimedOut {
		fmt.Fprintln(os.Stderr, "Error: command timed out")
	}
	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
}

// splitForkArg parses a cp argument into a possible "<fork-id>:<path>" form.
// The fork side is recognized syntactically: a colon is present and the
// segment before the FIRST colon contains no '/'. Fork ids never contain
// slashes, and a host path with a colon in a directory component (e.g.
// /a/b:c) keeps its slash before the colon, so it is correctly read as a host
// path. (A bare relative host path like "we:rd.txt" is ambiguous, as with
// docker cp; qualify it as "./we:rd.txt".) Existence and liveness of the named
// fork are validated later by Copy, which owns the parked/destroyed message.
func splitForkArg(arg string) (fork, path string, isFork bool) {
	i := strings.IndexByte(arg, ':')
	if i <= 0 {
		return "", arg, false
	}
	if strings.ContainsRune(arg[:i], '/') {
		return "", arg, false
	}
	return arg[:i], arg[i+1:], true
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: waypoint <command> [args...]")
		fmt.Println("Commands:")
		fmt.Println("  init <work-directory> [--quiet] [--shell]    - Initialize environment")
		fmt.Println("  build <dockerfile-directory> [--quiet]       - Build environment from Dockerfile")
		fmt.Println("  checkpoint <session> <checkpoint-id>         - Snapshot main fork")
		fmt.Println("  fork <session> <checkpoint-id> [--id ID] [--n K] - Materialize live fork(s)")
		fmt.Println("  exec <session> <fork-id> -- <command>        - Execute command in a fork")
		fmt.Println("  snapshot <session> <fork-id> <checkpoint-id> [--park] - Snapshot a live fork (--park: don't resume it)")
		fmt.Println("  create <session> <checkpoint-id>             - Legacy alias for checkpoint")
		fmt.Println("  fork-exec <session> <fork-id> <command>      - Legacy alias for exec")
		fmt.Println("  destroy <session> <fork-id>                  - Destroy a live fork")
		fmt.Println("  list <session> [--json]                      - List checkpoints and forks")
		fmt.Println("  suspend <session>                            - End all live forks; keep checkpoints on disk")
		fmt.Println("  cp <session> <fork>:<path> <host> | <host> <fork>:<path> - Copy a file/dir in or out of a live fork")
		fmt.Println("  cleanup <session> [--force]                  - Clean up session")
		fmt.Println("  info [session [checkpoint-id]]               - Show system, session, or checkpoint info")
		fmt.Println("  version                                      - Show version")
		fmt.Println()
		fmt.Printf("Version: %s, DAPLab\n", Version)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "__waypoint_restore_fork_child":
		if err := waypoint.RunForkRestoreChildFromArgs(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}

	case "__waypoint_flush_images":
		if err := waypoint.RunImageFlushFromArgs(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}

	case "init":
		if len(os.Args) < 3 {
			fmt.Println("Usage: init <work-directory> [--quiet] [--shell]")
			fmt.Println("  --shell: Start a shell in the initialized environment after setup")
			os.Exit(1)
		}
		workDir := os.Args[2]

		// Parse flags
		quiet := false
		shell := false

		for i := 3; i < len(os.Args); i++ {
			arg := os.Args[i]
			switch arg {
			case "--quiet":
				quiet = true
			case "--shell":
				shell = true
			default:
				fmt.Printf("Error: unknown flag: %s\n", arg)
				os.Exit(1)
			}
		}

		// Create a new manager with a random session
		manager, sessionID, err := waypoint.NewManagerWithSession()
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			os.Exit(1)
		}

		stagedDir, err := manager.StageEnvironment(workDir)
		if err != nil {
			fmt.Printf("Error staging environment: %v\n", err)
			os.Exit(1)
		}
		overlayPath, err := manager.InitEnvironment(stagedDir)
		if err != nil {
			fmt.Printf("Error initializing environment: %v\n", err)
			os.Exit(1)
		}

		shellPid := 0
		socketPath := ""
		if shell {
			shellPid, socketPath, err = manager.StartShell(overlayPath)
			if err != nil {
				fmt.Printf("Error starting shell: %v\n", err)
				os.Exit(1)
			}
		}

		if quiet {
			fmt.Printf("%s,%s\n", sessionID, overlayPath)
		} else {
			fmt.Printf("Environment initialized!\n")
			fmt.Printf("Session ID: %s\n", sessionID)
			fmt.Printf("Work in this directory: %s\n", overlayPath)
			if shell {
				fmt.Printf("Shell PID: %d [socket: %s]\n", shellPid, socketPath)
			}
			fmt.Printf("\nSave the session ID for future operations!\n")
		}

	case "build":
		if len(os.Args) < 3 {
			fmt.Println("Usage: build <dockerfile-directory> [--quiet]")
			os.Exit(1)
		}
		dockerfileDir := os.Args[2]

		// Parse flags
		quiet := false

		for i := 3; i < len(os.Args); i++ {
			arg := os.Args[i]
			switch arg {
			case "--quiet":
				quiet = true
			default:
				fmt.Printf("Error: unknown flag: %s\n", arg)
				os.Exit(1)
			}
		}

		// Create a new manager with a random session
		manager, sessionID, err := waypoint.NewManagerWithSession()
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			os.Exit(1)
		}

		overlayPath, bashPid, err := manager.BuildEnvironment(dockerfileDir, quiet)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error building sandbox image: %v\n", err)
			os.Exit(1)
		}

		if quiet {
			fmt.Printf("%s,%s,%d\n", sessionID, overlayPath, bashPid)
		} else {
			fmt.Printf("Sandbox environment built successfully!\n")
			fmt.Printf("Session ID: %s\n", sessionID)
			fmt.Printf("Work in this directory: %s\n", overlayPath)
			fmt.Printf("Sandbox bash PID: %d\n", bashPid)
			fmt.Printf("\nSave the session ID for future operations!\n")
		}

	case "checkpoint", "create":
		if len(os.Args) != 4 {
			fmt.Printf("Usage: %s <session> <checkpoint-id>\n", os.Args[1])
			os.Exit(1)
		}
		sessionID := os.Args[2]
		checkpointID := os.Args[3]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		ckpt, err := manager.SnapshotFork(waypoint.MainForkID, checkpointID)
		if err != nil {
			fmt.Printf("Error creating checkpoint: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Checkpoint '%s' created successfully\n", checkpointID)
		if ckpt.Metadata != nil && ckpt.Metadata.Snapshot != nil {
			fmt.Printf("phases %s\n", ckpt.Metadata.Snapshot)
		}

	case "fork":
		if len(os.Args) < 4 {
			fmt.Println("Usage: fork <session> <checkpoint-id> [--id ID] [--n K]")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		checkpointID := os.Args[3]
		count := 1
		forkID := ""
		for i := 4; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "--id":
				if i+1 >= len(os.Args) {
					fmt.Println("Error: --id requires a value")
					os.Exit(1)
				}
				forkID = os.Args[i+1]
				i++
			case "--n":
				if i+1 >= len(os.Args) {
					fmt.Println("Error: --n requires a value")
					os.Exit(1)
				}
				parsed, err := strconv.Atoi(os.Args[i+1])
				if err != nil || parsed <= 0 {
					fmt.Printf("Invalid fork count: %s\n", os.Args[i+1])
					os.Exit(1)
				}
				count = parsed
				i++
			default:
				fmt.Printf("Error: unknown flag: %s\n", os.Args[i])
				os.Exit(1)
			}
		}
		if forkID != "" && count != 1 {
			fmt.Println("Error: --id can only be used when creating one fork")
			os.Exit(1)
		}

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		for i := 0; i < count; i++ {
			spec := waypoint.ForkSpec{}
			if forkID != "" {
				spec.ID = forkID
			}
			f, err := manager.ForkCheckpoint(checkpointID, spec)
			if err != nil {
				fmt.Printf("Error creating fork: %v\n", err)
				os.Exit(1)
			}
			line := fmt.Sprintf("%s pid=%d socket=%s duration=%s", f.ID, f.PID, f.SocketPath, f.RestoreDuration)
			if f.RestoreBreakdown != nil {
				line += " " + f.RestoreBreakdown.String()
			}
			fmt.Println(line)
		}

	case "fork-exec":
		if len(os.Args) < 5 {
			fmt.Println("Usage: fork-exec <session> <fork-id> <command> [args...]")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		forkID := os.Args[3]
		command := os.Args[4]
		args := os.Args[5:]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		result, err := manager.ExecuteForkCommand(forkID, command, args...)
		if err != nil {
			fmt.Printf("Error executing fork command: %v\n", err)
			os.Exit(1)
		}
		printExecResult(result)

	case "destroy":
		if len(os.Args) != 4 {
			fmt.Println("Usage: destroy <session> <fork-id>")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		forkID := os.Args[3]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		if err := manager.DestroyFork(forkID); err != nil {
			fmt.Printf("Error destroying fork: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Fork '%s' destroyed successfully\n", forkID)

	case "snapshot":
		park := false
		args := []string{}
		for _, a := range os.Args[2:] {
			if a == "--park" {
				park = true
			} else {
				args = append(args, a)
			}
		}
		if len(args) != 3 {
			fmt.Println("Usage: snapshot <session> <fork-id> <checkpoint-id> [--park]")
			os.Exit(1)
		}
		sessionID, forkID, checkpointID := args[0], args[1], args[2]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		var ckpt *waypoint.Checkpoint
		if park {
			ckpt, err = manager.ParkFork(forkID, checkpointID)
		} else {
			ckpt, err = manager.SnapshotFork(forkID, checkpointID)
		}
		if err != nil {
			fmt.Printf("Error snapshotting fork: %v\n", err)
			os.Exit(1)
		}
		if park {
			fmt.Printf("Fork '%s' parked as checkpoint '%s'\n", forkID, checkpointID)
		} else {
			fmt.Printf("Fork '%s' snapshotted as checkpoint '%s'\n", forkID, checkpointID)
		}
		if ckpt.Metadata != nil && ckpt.Metadata.Snapshot != nil {
			fmt.Printf("phases %s\n", ckpt.Metadata.Snapshot)
		}

	case "exec":
		if len(os.Args) < 6 || os.Args[4] != "--" {
			fmt.Println("Usage: exec <session> <fork-id> -- <command>")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		forkID := os.Args[3]
		command := strings.Join(os.Args[5:], " ")

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		result, err := manager.ExecuteForkCommand(forkID, command)
		if err != nil {
			fmt.Printf("Error executing command: %v\n", err)
			os.Exit(1)
		}
		printExecResult(result)

	case "list":
		if len(os.Args) > 4 {
			fmt.Println("Usage: list [session] [--json]")
			os.Exit(1)
		}

		if len(os.Args) == 2 {
			sessions, err := waypoint.ListSessions()
			if err != nil {
				fmt.Printf("Error listing sessions: %v\n", err)
				os.Exit(1)
			}
			if len(sessions) == 0 {
				fmt.Println("No sessions found")
			} else {
				fmt.Println("Available sessions:")
				for _, session := range sessions {
					fmt.Printf("  %s\n", session)
				}
			}
			break
		}

		sessionID := os.Args[2]
		asJSON := len(os.Args) > 3 && os.Args[3] == "--json"

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		listing, err := manager.ListSession()
		if err != nil {
			fmt.Printf("Error listing session: %v\n", err)
			os.Exit(1)
		}
		if asJSON {
			data, err := json.MarshalIndent(listing, "", "  ")
			if err != nil {
				fmt.Printf("Error encoding listing: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(string(data))
			break
		}
		if len(listing.Checkpoints) == 0 {
			fmt.Println("No checkpoints found")
		} else {
			fmt.Println("Checkpoints:")
			for _, cp := range listing.Checkpoints {
				fmt.Printf("  %s parent=%s status=%s layers=%s\n", cp.ID, cp.ParentID, cp.Status, strings.Join(cp.LayerIDs, ","))
			}
		}
		if len(listing.Forks) > 0 {
			fmt.Println("Live forks:")
			for _, f := range listing.Forks {
				fmt.Printf("  %s checkpoint=%s status=%s pid=%d socket=%s\n", f.ID, f.BaseCheckpointID, f.Status, f.PID, f.SocketPath)
			}
		}

	case "suspend":
		if len(os.Args) < 3 {
			fmt.Println("Usage: suspend <session>")
			os.Exit(1)
		}
		manager, err := waypoint.LoadManager(os.Args[2])
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		if err := manager.Suspend(); err != nil {
			fmt.Printf("Error suspending session: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Session '%s' suspended; checkpoints remain on disk — `waypoint fork` any of them to resume\n", os.Args[2])

	case "cp":
		if len(os.Args) != 5 {
			fmt.Println("Usage: cp <session> <fork-id>:<path> <host-path>   (copy out)")
			fmt.Println("       cp <session> <host-path> <fork-id>:<path>   (copy in)")
			fmt.Println("  Exactly one argument carries a <fork-id>: prefix.")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		srcFork, srcPath, srcIsFork := splitForkArg(os.Args[3])
		dstFork, dstPath, dstIsFork := splitForkArg(os.Args[4])
		switch {
		case srcIsFork && !dstIsFork:
			if err := manager.Copy(srcFork, srcPath, dstPath, waypoint.CopyOut); err != nil {
				fmt.Printf("Error copying out of fork: %v\n", err)
				os.Exit(1)
			}
		case !srcIsFork && dstIsFork:
			if err := manager.Copy(dstFork, dstPath, srcPath, waypoint.CopyIn); err != nil {
				fmt.Printf("Error copying into fork: %v\n", err)
				os.Exit(1)
			}
		case srcIsFork && dstIsFork:
			fmt.Println("Error: fork-to-fork copy is not supported; copy via the host")
			os.Exit(1)
		default:
			fmt.Println("Error: exactly one path must carry a <fork-id>: prefix")
			os.Exit(1)
		}

	case "cleanup":
		if len(os.Args) < 3 {
			fmt.Println("Usage: cleanup <session> [--force]")
			os.Exit(1)
		}
		sessionID := os.Args[2]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		force := len(os.Args) > 3 && os.Args[3] == "--force"

		if force {
			if err := manager.CleanupForce(); err != nil {
				fmt.Printf("Error cleaning up session forcefully: %v\n", err)
				os.Exit(1)
			}
		} else {
			if err := manager.CleanupInteractive(); err != nil {
				fmt.Printf("Error cleaning up session: %v\n", err)
				fmt.Printf("Try: sudo waypoint cleanup %s --force\n", sessionID)
				os.Exit(1)
			}
		}
		fmt.Printf("Session '%s' cleaned up successfully\n", sessionID)

	case "info":
		if len(os.Args) > 4 {
			fmt.Println("Usage: info [session [checkpoint-id]]")
			os.Exit(1)
		}
		if err := printInfo(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error collecting info: %v\n", err)
			os.Exit(1)
		}

	case "version", "--version", "-v":
		fmt.Printf("waypoint version %s\n", Version)

	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
