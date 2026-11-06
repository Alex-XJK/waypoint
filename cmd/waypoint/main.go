package main

// waypoint: A lightweight process checkpointing and restoration tool
//
// GitHub Repository: https://github.com/Alex-XJK/waypoint.git
// Designed and developed by Alex Jiakai Xu (https://alex-xjk.github.io/), DAPLab @ Columbia University (https://daplab.cs.columbia.edu/)

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Alex-XJK/waypoint/pkg/group"
	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

var Version = "v0.7.1"

// printExecResult writes the command output to stdout and exits with the
// command's own exit code.
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

// execUsage is the usage line for `exec`.
const execUsage = "Usage: exec <session> <fork-id> -- '<bash command line>'"

// parseExecCommand returns the bash command line from the arguments after
// `exec <session> <fork-id> --`. Exactly one argument is accepted; several
// are refused rather than joined, which would lose the caller's quoting.
func parseExecCommand(rest []string) (string, error) {
	switch len(rest) {
	case 0:
		return "", fmt.Errorf("%s\n       exec takes one argument after --: the bash command line", execUsage)
	case 1:
		return rest[0], nil
	default:
		return "", fmt.Errorf("exec takes one command string; got %d arguments\n"+
			"       Quote the whole command so the fork's shell parses it: -- %s",
			len(rest), shellQuote(strings.Join(rest, " ")))
	}
}

// shellQuote renders s as one POSIX single-quoted word, for the hint above.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runExec runs command in the fork and exits with the command's status.
// A syntax error is reported on stderr with exit status 2, as bash does.
func runExec(sessionID, forkID, command string) {
	manager, err := waypoint.LoadManager(sessionID)
	if err != nil {
		fmt.Printf("Error loading session: %v\n", err)
		os.Exit(1)
	}
	result, err := manager.ExecuteForkCommand(forkID, command)
	if errors.Is(err, waypoint.ErrCommandSyntax) {
		fmt.Fprintf(os.Stderr, "waypoint exec: %v\n", err)
		os.Exit(2)
	}
	if err != nil {
		fmt.Printf("Error executing command: %v\n", err)
		os.Exit(1)
	}
	printExecResult(result)
}

// attachUsage describes the --attach value accepted by init, build, and fork.
const attachUsage = "--attach <fabric-netns>=<ipv4/prefix>: connect the fork to a fabric's bridge " + waypoint.FabricBridge + " (private network mode)"

// parseAttach parses an --attach value. Addresses never contain '=', so the
// last one separates them from the namespace path.
func parseAttach(value string) (waypoint.NetworkAttachment, error) {
	i := strings.LastIndex(value, "=")
	if i <= 0 || i == len(value)-1 {
		return waypoint.NetworkAttachment{}, fmt.Errorf("--attach wants <fabric-netns>=<ipv4/prefix>, got %q", value)
	}
	return waypoint.NetworkAttachment{FabricNS: value[:i], Address: value[i+1:]}, nil
}

// attachFlag reads the value of the --attach flag at args[i].
func attachFlag(args []string, i int) waypoint.NetworkAttachment {
	if i+1 >= len(args) {
		fmt.Println("Error: --attach requires a value")
		os.Exit(1)
	}
	a, err := parseAttach(args[i+1])
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	if group.IsProjectFabric(a.FabricNS) {
		fmt.Printf("Error: %s is a compose project's fabric; its addresses belong to the project's replicas\n", a.FabricNS)
		os.Exit(1)
	}
	return a
}

// checkAttach validates an --attach value before a session is created, so a
// bad value leaves nothing behind.
func checkAttach(a *waypoint.NetworkAttachment) {
	if a == nil {
		return
	}
	if err := waypoint.CheckNetworkAttachment(*a); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// loadStandaloneSession loads a session for a command that changes its forks,
// checkpoints, or lifetime. A compose project's member session is refused:
// its forks are replicas shared with the other services, so changing one
// member alone desynchronizes the project, and a lone fork either fails to
// restore its connections or collides with a live replica's addresses.
// Members of a project that no longer exists are standalone again.
func loadStandaloneSession(command, sessionID string) *waypoint.Manager {
	manager, err := waypoint.LoadManager(sessionID)
	if err != nil {
		fmt.Printf("Error loading session: %v\n", err)
		os.Exit(1)
	}
	project, _ := manager.Group()
	if project == "" {
		return manager
	}
	service, err := group.MemberService(project, sessionID)
	if err != nil {
		fmt.Printf("Error checking compose project %s: %v\n", project, err)
		os.Exit(1)
	}
	if service != "" {
		fmt.Printf("Error: session %s is service %q of compose project %s; use `waypoint compose %s %s ...` instead\n",
			sessionID, service, project, command, project)
		os.Exit(1)
	}
	return manager
}

// attachMain applies an --attach value to a new session's main fork.
func attachMain(manager *waypoint.Manager, a *waypoint.NetworkAttachment) {
	if a == nil {
		return
	}
	if err := manager.AttachMain(*a); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

type forkResult struct {
	fork *waypoint.Fork
	err  error
}

// forkCheckpointsConcurrently starts every requested materialization at once
// and stores each result in its request slot so callers can print stable output.
func forkCheckpointsConcurrently(count int, create func(int) (*waypoint.Fork, error)) []forkResult {
	results := make([]forkResult, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			results[i].fork, results[i].err = create(i)
		}(i)
	}
	wg.Wait()
	return results
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: waypoint <command> [args...]")
		fmt.Println("Commands:")
		fmt.Println("  init <work-directory> [--quiet] [--shell] [--attach NS=ADDR] - Initialize environment")
		fmt.Println("  build <dockerfile-directory> [--quiet] [--attach NS=ADDR] - Build environment from Dockerfile")
		fmt.Println("  checkpoint <session> <checkpoint-id>         - Snapshot main fork")
		fmt.Println("  fork <session> <checkpoint-id> [--id ID] [--n K] [--attach NS=ADDR] - Materialize live fork(s)")
		fmt.Println("  exec <session> <fork-id> -- '<command>'      - Run one bash command line in a fork")
		fmt.Println("  cp <session> <source> <destination>          - Copy host path <-> fork-id:/path")
		fmt.Println("  snapshot <session> <fork-id> <checkpoint-id> [--park] - Snapshot a live fork (--park: don't resume it)")
		fmt.Println("  create <session> <checkpoint-id>             - Legacy alias for checkpoint")
		fmt.Println("  fork-exec <session> <fork-id> <command>      - Legacy alias for exec")
		fmt.Println("  destroy <session> <fork-id>                  - Destroy a live fork")
		fmt.Println("  list <session> [--json]                      - List checkpoints and forks")
		fmt.Println("  suspend <session>                            - End all live forks; keep checkpoints on disk")
		fmt.Println("  cleanup <session> [--force]                  - Clean up session")
		fmt.Println("  info [session [checkpoint-id]]               - Show system, session, or checkpoint info")
		fmt.Println("  compose <command> [args...]                  - Multi-session projects from compose files")
		fmt.Println("  version                                      - Show version")
		fmt.Println()
		fmt.Printf("Version: %s, DAPLab\n", Version)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "__waypoint_criu_action":
		if err := waypoint.RunCriuAction(); err != nil {
			fmt.Fprintf(os.Stderr, "criu action hook: %v\n", err)
		}
		return
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
			fmt.Println("Usage: init <work-directory> [--quiet] [--shell] [--attach NS=ADDR]")
			fmt.Println("  --shell: Start a shell in the initialized environment after setup")
			fmt.Println("  " + attachUsage + "; needs --shell")
			os.Exit(1)
		}
		workDir := os.Args[2]

		// Parse flags
		quiet := false
		shell := false
		var attach *waypoint.NetworkAttachment

		for i := 3; i < len(os.Args); i++ {
			arg := os.Args[i]
			switch arg {
			case "--quiet":
				quiet = true
			case "--shell":
				shell = true
			case "--attach":
				a := attachFlag(os.Args, i)
				attach = &a
				i++
			default:
				fmt.Printf("Error: unknown flag: %s\n", arg)
				os.Exit(1)
			}
		}
		if attach != nil && !shell {
			fmt.Println("Error: --attach requires --shell")
			os.Exit(1)
		}
		checkAttach(attach)

		// Create a new manager with a random session
		manager, sessionID, err := waypoint.NewManagerWithSession()
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			os.Exit(1)
		}
		attachMain(manager, attach)

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
			fmt.Println("Usage: build <dockerfile-directory> [--quiet] [--attach NS=ADDR]")
			fmt.Println("  " + attachUsage)
			os.Exit(1)
		}
		dockerfileDir := os.Args[2]

		// Parse flags
		quiet := false
		var attach *waypoint.NetworkAttachment

		for i := 3; i < len(os.Args); i++ {
			arg := os.Args[i]
			switch arg {
			case "--quiet":
				quiet = true
			case "--attach":
				a := attachFlag(os.Args, i)
				attach = &a
				i++
			default:
				fmt.Printf("Error: unknown flag: %s\n", arg)
				os.Exit(1)
			}
		}
		checkAttach(attach)

		// Create a new manager with a random session
		manager, sessionID, err := waypoint.NewManagerWithSession()
		if err != nil {
			fmt.Printf("Error creating session: %v\n", err)
			os.Exit(1)
		}
		attachMain(manager, attach)

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

		manager := loadStandaloneSession("checkpoint", sessionID)

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
			fmt.Println("Usage: fork <session> <checkpoint-id> [--id ID] [--n K] [--attach NS=ADDR]")
			fmt.Println("  " + attachUsage + "; one fork only")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		checkpointID := os.Args[3]
		count := 1
		forkID := ""
		var attach *waypoint.NetworkAttachment
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
			case "--attach":
				a := attachFlag(os.Args, i)
				attach = &a
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
		if attach != nil && count != 1 {
			// Copies of a fork on one fabric would share its address.
			fmt.Println("Error: --attach can only be used when creating one fork")
			os.Exit(1)
		}

		manager := loadStandaloneSession("fork", sessionID)
		results := forkCheckpointsConcurrently(count, func(_ int) (*waypoint.Fork, error) {
			spec := waypoint.ForkSpec{Attach: attach}
			if forkID != "" {
				spec.ID = forkID
			}
			return manager.ForkCheckpoint(checkpointID, spec)
		})
		failed := false
		for _, result := range results {
			if result.err != nil {
				fmt.Printf("Error creating fork: %v\n", result.err)
				failed = true
				continue
			}
			f := result.fork
			line := fmt.Sprintf("%s pid=%d socket=%s duration=%s", f.ID, f.PID, f.SocketPath, f.RestoreDuration)
			if f.RestoreBreakdown != nil {
				line += " " + f.RestoreBreakdown.String()
			}
			fmt.Println(line)
		}
		if failed {
			os.Exit(1)
		}

	case "compose":
		runCompose(os.Args[2:])

	case "fork-exec":
		// Legacy alias: same contract as exec, minus the `--`.
		if len(os.Args) < 4 {
			fmt.Println("Usage: fork-exec <session> <fork-id> '<bash command line>'")
			os.Exit(1)
		}
		command, err := parseExecCommand(os.Args[4:])
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		runExec(os.Args[2], os.Args[3], command)

	case "destroy":
		if len(os.Args) != 4 {
			fmt.Println("Usage: destroy <session> <fork-id>")
			os.Exit(1)
		}
		sessionID := os.Args[2]
		forkID := os.Args[3]

		manager := loadStandaloneSession("destroy", sessionID)
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

		manager := loadStandaloneSession("snapshot", sessionID)
		var ckpt *waypoint.Checkpoint
		var err error
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
		if len(os.Args) < 5 || os.Args[4] != "--" {
			fmt.Println(execUsage)
			os.Exit(1)
		}
		command, err := parseExecCommand(os.Args[5:])
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		runExec(os.Args[2], os.Args[3], command)

	case "list":
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
		asJSON := false
		for i := 3; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "--json":
				asJSON = true
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
				line := fmt.Sprintf("  %s checkpoint=%s status=%s pid=%d socket=%s", f.ID, f.BaseCheckpointID, f.Status, f.PID, f.SocketPath)
				if f.Attachment != nil {
					line += fmt.Sprintf(" fabric=%s addr=%s", f.Attachment.FabricNS, f.Attachment.Address)
				}
				fmt.Println(line)
			}
		}

	case "suspend":
		if len(os.Args) != 3 {
			fmt.Println("Usage: suspend <session>")
			os.Exit(1)
		}
		manager := loadStandaloneSession("suspend", os.Args[2])
		if err := manager.Suspend(); err != nil {
			fmt.Printf("Error suspending session: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Session '%s' suspended; checkpoints remain on disk — `waypoint fork` any of them to resume\n", os.Args[2])

	case "cleanup":
		if len(os.Args) < 3 {
			fmt.Println("Usage: cleanup <session> [--force]")
			os.Exit(1)
		}
		sessionID := os.Args[2]

		force := false
		for i := 3; i < len(os.Args); i++ {
			switch os.Args[i] {
			case "--force":
				force = true
			default:
				fmt.Printf("Error: unknown flag: %s\n", os.Args[i])
				os.Exit(1)
			}
		}

		manager := loadStandaloneSession("cleanup", sessionID)

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

	case "cp":
		if err := runCopy(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error copying: %v\n", err)
			os.Exit(1)
		}

	case "version", "--version", "-v":
		fmt.Printf("waypoint version %s\n", Version)

	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
