package main

// `waypoint compose`: compose projects, one session per service, whose
// replicas are forked and snapshotted together (see pkg/group).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Alex-XJK/waypoint/pkg/compose"
	"github.com/Alex-XJK/waypoint/pkg/group"
	"github.com/Alex-XJK/waypoint/pkg/waypoint"
)

const composeUsage = `Usage: waypoint compose <command> [args...]
Commands:
  up [-f FILE]... [-p NAME] [--quiet]               - Start a project from a compose file
  checkpoint <project> <checkpoint-id>              - Snapshot the main replica
  snapshot <project> <replica> <checkpoint-id> [--park] - Snapshot a replica (--park: don't resume it)
  fork <project> <checkpoint-id> [--id ID] [--n K]  - Materialize live replica(s)
  exec <project> <replica> <service> -- '<command>' - Run one bash command line in a service
  destroy <project> <replica>                       - Destroy a replica
  list [project] [--json]                           - List projects, or a project's replicas and checkpoints
  suspend <project>                                 - End all replicas; keep checkpoints
  cleanup <project> [--force]                       - Remove the project and its sessions`

func composeFail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func composeArgs(args []string, n int, usage string) {
	if len(args) != n {
		fmt.Println("Usage: compose " + usage)
		os.Exit(1)
	}
}

func runCompose(args []string) {
	if len(args) == 0 {
		fmt.Println(composeUsage)
		os.Exit(1)
	}
	group.Logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format, a...) }
	cmd, args := args[0], args[1:]
	switch cmd {
	case "up":
		var files []string
		name := ""
		quiet := false
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-f", "--file", "-p", "--project-name":
				if i+1 >= len(args) {
					composeFail("%s requires a value", args[i])
				}
				if args[i] == "-f" || args[i] == "--file" {
					files = append(files, args[i+1])
				} else {
					name = args[i+1]
				}
				i++
			case "--quiet":
				quiet = true
			default:
				composeFail("unknown flag: %s", args[i])
			}
		}
		if quiet {
			group.Logf = func(string, ...any) {}
		}
		project, err := compose.Load(files, name)
		if err != nil {
			composeFail("%v", err)
		}
		defer project.Close()
		for _, w := range project.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
		rec, err := group.Up(project.Spec)
		if err != nil {
			project.Close()
			composeFail("%v", err)
		}
		if quiet {
			fmt.Println(rec.Name)
			return
		}
		fmt.Printf("Project %s is up on fabric subnet %s\n", rec.Name, rec.Subnet)
		for _, s := range rec.Services {
			fmt.Printf("  %s session=%s addr=%s\n", s.Name, s.SessionID, s.Address)
		}

	case "checkpoint":
		composeArgs(args, 2, "checkpoint <project> <checkpoint-id>")
		if _, err := group.Snapshot(args[0], group.MainReplica, args[1], false); err != nil {
			composeFail("%v", err)
		}
		fmt.Printf("Checkpoint '%s' created successfully\n", args[1])

	case "snapshot":
		if len(args) == 4 && args[3] == "--park" {
			if _, err := group.Snapshot(args[0], args[1], args[2], true); err != nil {
				composeFail("%v", err)
			}
			fmt.Printf("Replica '%s' parked as checkpoint '%s'\n", args[1], args[2])
			return
		}
		composeArgs(args, 3, "snapshot <project> <replica> <checkpoint-id> [--park]")
		if _, err := group.Snapshot(args[0], args[1], args[2], false); err != nil {
			composeFail("%v", err)
		}
		fmt.Printf("Replica '%s' snapshotted as checkpoint '%s'\n", args[1], args[2])

	case "fork":
		if len(args) < 2 {
			fmt.Println("Usage: compose fork <project> <checkpoint-id> [--id ID] [--n K]")
			os.Exit(1)
		}
		var ids []string
		count := 1
		for i := 2; i < len(args); i++ {
			if i+1 >= len(args) {
				composeFail("%s requires a value", args[i])
			}
			switch args[i] {
			case "--id":
				ids = append(ids, args[i+1])
			case "--n":
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n <= 0 {
					composeFail("invalid replica count: %s", args[i+1])
				}
				count = n
			default:
				composeFail("unknown flag: %s", args[i])
			}
			i++
		}
		if len(ids) > 1 || (len(ids) == 1 && count != 1) {
			composeFail("--id can only be used when creating one replica")
		}
		replicas, err := group.Fork(args[0], args[1], ids, count)
		for _, r := range replicas {
			fmt.Println(r.ID)
		}
		if err != nil {
			composeFail("%v", err)
		}

	case "exec":
		if len(args) < 4 || args[3] != "--" {
			fmt.Println("Usage: compose exec <project> <replica> <service> -- '<bash command line>'")
			os.Exit(1)
		}
		command, err := parseExecCommand(args[4:])
		if err != nil {
			composeFail("%v", err)
		}
		result, err := group.Exec(args[0], args[1], args[2], command)
		if errors.Is(err, waypoint.ErrCommandSyntax) {
			fmt.Fprintf(os.Stderr, "waypoint compose exec: %v\n", err)
			os.Exit(2)
		}
		if err != nil {
			composeFail("%v", err)
		}
		printExecResult(result)

	case "destroy":
		composeArgs(args, 2, "destroy <project> <replica>")
		if err := group.Destroy(args[0], args[1]); err != nil {
			composeFail("%v", err)
		}
		fmt.Printf("Replica '%s' destroyed\n", args[1])

	case "list":
		asJSON := len(args) > 0 && args[len(args)-1] == "--json"
		if asJSON {
			args = args[:len(args)-1]
		}
		if len(args) == 0 {
			names, err := group.ListProjects()
			if err != nil {
				composeFail("%v", err)
			}
			if asJSON {
				printJSON(names)
				return
			}
			if len(names) == 0 {
				fmt.Println("No projects found")
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return
		}
		composeArgs(args, 1, "list [project] [--json]")
		listing, err := group.List(args[0])
		if err != nil {
			composeFail("%v", err)
		}
		if asJSON {
			printJSON(listing)
			return
		}
		fmt.Printf("Project %s (%s, subnet %s)\n", listing.Record.Name, listing.Record.Status, listing.Record.Subnet)
		fmt.Println("Services:")
		for _, s := range listing.Record.Services {
			fmt.Printf("  %s session=%s addr=%s\n", s.Name, s.SessionID, s.Address)
		}
		if len(listing.Checkpoints) > 0 {
			fmt.Println("Checkpoints:")
			for _, c := range listing.Checkpoints {
				fmt.Printf("  %s parent=%s from=%s status=%s\n", c.ID, c.Parent, c.FromReplica, c.Status)
			}
		}
		if len(listing.Replicas) > 0 {
			fmt.Println("Replicas:")
			for _, r := range listing.Replicas {
				fmt.Printf("  %s checkpoint=%s status=%s\n", r.ID, r.Checkpoint, r.Status)
			}
		}

	case "suspend":
		composeArgs(args, 1, "suspend <project>")
		if err := group.Suspend(args[0]); err != nil {
			composeFail("%v", err)
		}
		fmt.Printf("Project '%s' suspended; fork a checkpoint to continue\n", args[0])

	case "cleanup":
		force := len(args) == 2 && args[1] == "--force"
		if !force {
			composeArgs(args, 1, "cleanup <project> [--force]")
		}
		if err := group.Cleanup(args[0], force); err != nil {
			composeFail("%v", err)
		}
		fmt.Printf("Project '%s' cleaned up\n", args[0])

	default:
		fmt.Println(composeUsage)
		os.Exit(1)
	}
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		composeFail("encoding output: %v", err)
	}
	fmt.Println(strings.TrimRight(string(data), "\n"))
}
