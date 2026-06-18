package main

// waypoint: A lightweight process checkpointing and restoration tool
//
// GitHub Repository: https://github.com/Alex-XJK/waypoint.git
// Designed and developed by Alex Jiakai Xu (https://alex-xjk.github.io/), DAPLab @ Columbia University (https://daplab.cs.columbia.edu/)

import (
	"fmt"
	"os"
	"strconv"

	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

var Version = "v0.6.0"

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: waypoint <command> [args...]")
		fmt.Println("Commands:")
		fmt.Println("  init <work-directory> [--quiet] [--shell]    - Initialize environment")
		fmt.Println("  build <dockerfile-directory> [--quiet]       - Build environment from Dockerfile")
		fmt.Println("  create <session> <checkpoint-id> [pid | -1]  - Create checkpoint")
		fmt.Println("  restore <session> <checkpoint-id>            - Restore checkpoint")
		fmt.Println("  fork <session> <checkpoint-id> [--n K]       - Materialize live fork(s)")
		fmt.Println("  fork-exec <session> <fork-id> <command>      - Execute command in a fork")
		fmt.Println("  destroy <session> <fork-id>                  - Destroy a live fork")
		fmt.Println("  exec <session> <command> [args...]           - Execute command in environment")
		fmt.Println("  list <session>                               - List checkpoints")
		fmt.Println("  cleanup <session> [--force]                  - Clean up session")
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

		overlayPath, err := manager.InitEnvironment(workDir)
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

	case "create":
		if len(os.Args) < 4 {
			fmt.Println("Usage: create <session> <checkpoint-id> [pid | -1]")
			fmt.Println("  If pid not provided, checkpoint the shell if enabled; otherwise, skip memory checkpoint")
			fmt.Println("  Use -1 to force skip memory checkpoint")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		checkpointID := os.Args[3]

		pid := waypoint.PidNotProvided
		err := error(nil)
		if len(os.Args) > 4 {
			pid, err = strconv.Atoi(os.Args[4])
			if err != nil {
				fmt.Printf("Invalid PID: %s\n", os.Args[4])
				os.Exit(1)
			}
		}

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		if err := manager.CreateCheckpointNew(pid, checkpointID); err != nil {
			fmt.Printf("Error creating checkpoint: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Checkpoint '%s' created successfully\n", checkpointID)

	case "restore":
		if len(os.Args) != 4 {
			fmt.Println("Usage: restore <session> <checkpoint-id>")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		checkpointID := os.Args[3]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		newPID, err := manager.RestoreCheckpointNew(checkpointID)
		if err != nil {
			fmt.Printf("Error restoring checkpoint: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Checkpoint '%s' restored, new PID: %d\n", checkpointID, newPID)

	case "fork":
		if len(os.Args) < 4 {
			fmt.Println("Usage: fork <session> <checkpoint-id> [--n K] [--lazy-pages]")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		checkpointID := os.Args[3]
		count := 1
		lazyPages := false
		for i := 4; i < len(os.Args); i++ {
			switch os.Args[i] {
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
			case "--lazy-pages":
				lazyPages = true
			default:
				fmt.Printf("Error: unknown flag: %s\n", os.Args[i])
				os.Exit(1)
			}
		}

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}
		for i := 0; i < count; i++ {
			f, err := manager.ForkCheckpoint(checkpointID, waypoint.ForkSpec{LazyPages: lazyPages})
			if err != nil {
				fmt.Printf("Error creating fork: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("%s pid=%d socket=%s duration=%s\n", f.ID, f.PID, f.SocketPath, f.RestoreDuration)
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
		output, err := manager.ExecuteForkCommand(forkID, command, args...)
		if err != nil {
			fmt.Printf("Error executing fork command: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(output)

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

	case "exec":
		if len(os.Args) < 4 {
			fmt.Println("Usage: exec <session> <command> [args...]")
			fmt.Println("  Execute a command in the checkpoint environment")
			fmt.Println("  If shell enabled, command runs using the shell's sandbox; otherwise, it runs directly in the work overlay")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		command := os.Args[3]
		args := os.Args[4:]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		output, err := manager.ExecuteCommand(command, args...)
		if err != nil {
			fmt.Printf("Error executing command: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(output)

	case "list":
		if len(os.Args) != 3 {
			fmt.Println("Usage: list <session>")
			os.Exit(1)
		}
		sessionID := os.Args[2]

		manager, err := waypoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		checkpoints, err := manager.ListCheckpoints()
		if err != nil {
			fmt.Printf("Error listing checkpoints: %v\n", err)
			os.Exit(1)
		}
		if len(checkpoints) == 0 {
			fmt.Println("No checkpoints found")
		} else {
			fmt.Println("Available checkpoints:")
			for _, cp := range checkpoints {
				fmt.Printf("  %s\n", cp)
			}
		}
		forks, err := manager.ListForks()
		if err != nil {
			fmt.Printf("Error listing forks: %v\n", err)
			os.Exit(1)
		}
		if len(forks) > 0 {
			fmt.Println("Live forks:")
			for _, f := range forks {
				fmt.Printf("  %s checkpoint=%s status=%s pid=%d socket=%s\n", f.ID, f.CheckpointID, f.Status, f.PID, f.SocketPath)
			}
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
				fmt.Printf("Try: sudo ./waypoint cleanup %s --force\n", sessionID)
				os.Exit(1)
			}
		}
		fmt.Printf("Session '%s' cleaned up successfully\n", sessionID)

	case "version", "--version", "-v":
		fmt.Printf("waypoint version %s\n", Version)

	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
