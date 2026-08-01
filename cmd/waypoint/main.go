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

var Version = "v0.6.2"

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: waypoint <command> [args...]")
		fmt.Println("Commands:")
		fmt.Println("  init <work-directory> [--quiet] [--shell]    - Initialize environment")
		fmt.Println("  build <dockerfile-directory> [--quiet]       - Build environment from Dockerfile")
		fmt.Println("  create <session> <checkpoint-id> [pid | -1]  - Create checkpoint")
		fmt.Println("  restore <session> <checkpoint-id>            - Restore checkpoint")
		fmt.Println("  exec <session> <command> [args...]           - Execute command in environment")
		fmt.Println("  list [session]                               - List sessions or checkpoints")
		fmt.Println("  cleanup <session> [--force]                  - Clean up session")
		fmt.Println("  info [session [checkpoint-id]]               - Show system, session, or checkpoint info")
		fmt.Println("  version                                      - Show version")
		fmt.Println()
		fmt.Printf("Version: %s, DAPLab\n", Version)
		os.Exit(1)
	}

	switch os.Args[1] {
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
		if len(os.Args) > 3 {
			fmt.Println("Usage: list [session]")
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
