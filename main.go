package main

// checkpoint-lite: A lightweight process checkpointing and restoration tool
// Developed by Alex Xu, DAPLab @ Columbia University (https://daplab.cs.columbia.edu/)

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/Alex-XJK/checkpoint-lite/pkg/checkpoint"
)

var Version = "v0.4.1-dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: checkpoint-lite <command> [args...]")
		fmt.Println("Commands:")
		fmt.Println("  init <work-directory> [--quiet] [--sandbox]  - Initialize environment")
		fmt.Println("  create <session> <pid | -1> <checkpoint-id>  - Create checkpoint")
		fmt.Println("  restore <session> <checkpoint-id>            - Restore checkpoint")
		fmt.Println("  exec <session> <command> [args...]           - Execute command in environment")
		fmt.Println("  inject <session> <shell-pid> <command>       - Inject command into stateful shell")
		fmt.Println("  list <session>                               - List checkpoints")
		fmt.Println("  cleanup <session> [--force]                  - Clean up session")
		fmt.Println("  version                                      - Show version")
		fmt.Println()
		fmt.Printf("Version: %s, DAPLab\n", Version)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		if len(os.Args) < 3 {
			fmt.Println("Usage: init <work-directory> [--quiet] [--sandbox]")
			fmt.Println("  --sandbox: Enable lightweight sandbox isolation (default: disabled)")
			os.Exit(1)
		}
		workDir := os.Args[2]

		// Parse flags
		quiet := false
		sandboxMode := false

		for i := 3; i < len(os.Args); i++ {
			arg := os.Args[i]
			switch arg {
			case "--quiet":
				quiet = true
			case "--sandbox":
				sandboxMode = true
			default:
				fmt.Printf("Error: unknown flag: %s\n", arg)
				os.Exit(1)
			}
		}

		// Create a new manager with a random session
		manager, sessionID, err := checkpoint.NewManagerWithSession(sandboxMode)
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			os.Exit(1)
		}

		overlayPath, err := manager.InitEnvironment(workDir)
		if err != nil {
			fmt.Printf("Error initializing environment: %v\n", err)
			os.Exit(1)
		}

		if quiet {
			fmt.Printf("%s,%s\n", sessionID, overlayPath)
		} else {
			fmt.Printf("Environment initialized!\n")
			fmt.Printf("Session ID: %s\n", sessionID)
			fmt.Printf("Work in this directory: %s\n", overlayPath)
			if sandboxMode {
				fmt.Printf("Sandbox mode: enabled (light)\n")
			}
			fmt.Printf("\nSave the session ID for future operations!\n")
		}

	case "create":
		if len(os.Args) != 5 {
			fmt.Println("Usage: create <session> <pid | -1> <checkpoint-id>")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		pid, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fmt.Printf("Invalid PID: %s\n", os.Args[3])
			os.Exit(1)
		}
		checkpointID := os.Args[4]

		manager, err := checkpoint.LoadManager(sessionID)
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

		manager, err := checkpoint.LoadManager(sessionID)
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

	case "inject":
		if len(os.Args) != 5 {
			fmt.Println("Usage: inject <session> <shell-pid> <command>")
			fmt.Println("  Inject command into a stateful shell session")
			fmt.Println("  Use this to maintain shell state (env vars, etc.) across checkpoints")
			fmt.Println()
			fmt.Println("Example workflow:")
			fmt.Println("  1. Start a stateful shell: script -q -c \"bash --norc --noprofile\" /dev/null")
			fmt.Println("  2. Get shell PID: pgrep -n bash")
			fmt.Println("  3. Inject commands: checkpoint-lite inject <session> <pid> 'export VAR=value'")
			fmt.Println("  4. Create checkpoint: checkpoint-lite create <session> <pid> <checkpoint-id>")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		shellPID, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fmt.Printf("Invalid shell PID: %s\n", os.Args[3])
			os.Exit(1)
		}
		command := os.Args[4]

		manager, err := checkpoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		output, err := manager.InjectCommand(shellPID, command)
		if err != nil {
			fmt.Printf("Error injecting command: %v\n", err)
			if output != "" {
				fmt.Printf("Output:\n%s\n", output)
			}
			os.Exit(1)
		}

		// Print the captured output
		fmt.Print(output)

	case "exec":
		if len(os.Args) < 4 {
			fmt.Println("Usage: exec <session> <command> [args...]")
			fmt.Println("  Execute a command in the checkpoint environment")
			fmt.Println("  If sandbox mode is enabled, command runs in isolated sandbox")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		command := os.Args[3]
		args := os.Args[4:]

		manager, err := checkpoint.LoadManager(sessionID)
		if err != nil {
			fmt.Printf("Error loading session: %v\n", err)
			os.Exit(1)
		}

		cmd, err := manager.ExecuteCommand(command, args...)
		if err != nil {
			fmt.Printf("Error preparing command: %v\n", err)
			os.Exit(1)
		}

		// Connect stdin, stdout, stderr
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Run the command
		if err := cmd.Run(); err != nil {
			// Exit with the same code as the command
			if exitError, ok := err.(*exec.ExitError); ok {
				os.Exit(exitError.ExitCode())
			}
			fmt.Printf("Error executing command: %v\n", err)
			os.Exit(1)
		}

	case "list":
		if len(os.Args) != 3 {
			fmt.Println("Usage: list <session>")
			os.Exit(1)
		}
		sessionID := os.Args[2]

		manager, err := checkpoint.LoadManager(sessionID)
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

		manager, err := checkpoint.LoadManager(sessionID)
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
				fmt.Printf("Try: sudo ./checkpoint-lite cleanup %s --force\n", sessionID)
				os.Exit(1)
			}
		}
		fmt.Printf("Session '%s' cleaned up successfully\n", sessionID)

	case "version", "--version", "-v":
		fmt.Printf("checkpoint-lite version %s\n", Version)

	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
