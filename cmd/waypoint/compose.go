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

// composeCommands is the one description of each subcommand's syntax, for
// both the overview and a subcommand's own usage line.
var composeCommands = []struct{ name, args, help string }{
	{"up", "[-f FILE]... [-p NAME] [--quiet]", "Start a project from a compose file"},
	{"checkpoint", "<project> <checkpoint-id>", "Snapshot the main replica"},
	{"snapshot", "<project> <replica> <checkpoint-id> [--park]", "Snapshot a replica (--park: don't resume it)"},
	{"fork", "<project> <checkpoint-id> [--id ID] [--n K]", "Materialize live replica(s)"},
	{"exec", "<project> <replica> <service> -- '<command>'", "Run one bash command line in a service"},
	{"destroy", "<project> <replica>", "Destroy a replica"},
	{"list", "[project] [--json]", "List projects, or a project's replicas and checkpoints"},
	{"suspend", "<project>", "End all replicas; keep checkpoints"},
	{"cleanup", "<project> [--force]", "Remove the project and its sessions"},
}

// composeUsage prints cmd's usage line, or the overview for an unknown
// command, and exits.
func composeUsage(cmd string) {
	for _, c := range composeCommands {
		if c.name == cmd {
			fmt.Printf("Usage: compose %s %s\n", c.name, c.args)
			os.Exit(1)
		}
	}
	fmt.Println("Usage: waypoint compose <command> [args...]")
	fmt.Println("Commands:")
	for _, c := range composeCommands {
		fmt.Printf("  %-54s - %s\n", c.name+" "+c.args, c.help)
	}
	os.Exit(1)
}

// composeFail reports an error the way the session commands do, and exits.
func composeFail(err error) {
	fmt.Printf("Error: %v\n", err)
	os.Exit(1)
}

// composeFlag is a flag a subcommand accepts: the name its values are
// collected under (so aliases share one) and whether it takes a value.
type composeFlag struct {
	name  string
	value bool
}

// composeArgs splits a subcommand's arguments into positionals and flags,
// which may appear in any order. It exits with the usage line unless there
// are between minPos and maxPos positionals, and with an error for an
// unknown flag or a missing value. A flag without a value collects "".
func composeArgs(cmd string, args []string, minPos, maxPos int, flags map[string]composeFlag) ([]string, map[string][]string) {
	var pos []string
	got := map[string][]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		flag, known := flags[arg]
		switch {
		case known && flag.value:
			if i++; i >= len(args) {
				composeFail(fmt.Errorf("%s requires a value", arg))
			}
			got[flag.name] = append(got[flag.name], args[i])
		case known:
			got[flag.name] = append(got[flag.name], "")
		case strings.HasPrefix(arg, "-"):
			composeFail(fmt.Errorf("unknown flag: %s", arg))
		default:
			pos = append(pos, arg)
		}
	}
	if len(pos) < minPos || len(pos) > maxPos {
		composeUsage(cmd)
	}
	return pos, got
}

func runCompose(args []string) {
	if len(args) == 0 {
		composeUsage("")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "up":
		_, flags := composeArgs(cmd, args, 0, 0, map[string]composeFlag{
			"-f": {"file", true}, "--file": {"file", true},
			"-p": {"name", true}, "--project-name": {"name", true},
			"--quiet": {"quiet", false},
		})
		_, quiet := flags["quiet"]
		name := ""
		if names := flags["name"]; len(names) > 0 {
			name = names[len(names)-1]
		}
		var logf group.Logger
		if !quiet {
			logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format, a...) }
		}
		project, err := compose.Load(flags["file"], name)
		if err != nil {
			composeFail(err)
		}
		for _, w := range project.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
		rec, err := group.Up(project.Spec, logf)
		project.Close()
		if err != nil {
			composeFail(err)
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
		pos, _ := composeArgs(cmd, args, 2, 2, nil)
		if _, err := group.Snapshot(pos[0], group.MainReplica, pos[1], false); err != nil {
			composeFail(err)
		}
		fmt.Printf("Checkpoint '%s' created successfully\n", pos[1])

	case "snapshot":
		pos, flags := composeArgs(cmd, args, 3, 3, map[string]composeFlag{"--park": {"park", false}})
		_, park := flags["park"]
		if _, err := group.Snapshot(pos[0], pos[1], pos[2], park); err != nil {
			composeFail(err)
		}
		verb := "snapshotted"
		if park {
			verb = "parked"
		}
		fmt.Printf("Replica '%s' %s as checkpoint '%s'\n", pos[1], verb, pos[2])

	case "fork":
		pos, flags := composeArgs(cmd, args, 2, 2, map[string]composeFlag{"--id": {"id", true}, "--n": {"n", true}})
		ids, count := flags["id"], 1
		if counts := flags["n"]; len(counts) > 0 {
			n, err := strconv.Atoi(counts[len(counts)-1])
			if err != nil || n <= 0 {
				composeFail(fmt.Errorf("invalid replica count: %s", counts[len(counts)-1]))
			}
			count = n
		}
		if len(ids) > 1 || (len(ids) == 1 && count != 1) {
			composeFail(errors.New("--id can only be used when creating one replica"))
		}
		replicas, err := group.Fork(pos[0], pos[1], ids, count)
		for _, r := range replicas {
			fmt.Println(r.ID)
		}
		if err != nil {
			composeFail(err)
		}

	case "exec":
		// Everything after `--` is the command, never a flag.
		if len(args) < 4 || args[3] != "--" {
			composeUsage(cmd)
		}
		command, err := parseExecCommand(args[4:])
		if err != nil {
			composeFail(err)
		}
		result, err := group.Exec(args[0], args[1], args[2], command)
		if errors.Is(err, waypoint.ErrCommandSyntax) {
			fmt.Fprintf(os.Stderr, "waypoint compose exec: %v\n", err)
			os.Exit(2)
		}
		if err != nil {
			composeFail(err)
		}
		printExecResult(result)

	case "destroy":
		pos, _ := composeArgs(cmd, args, 2, 2, nil)
		if err := group.Destroy(pos[0], pos[1]); err != nil {
			composeFail(err)
		}
		fmt.Printf("Replica '%s' destroyed\n", pos[1])

	case "list":
		pos, flags := composeArgs(cmd, args, 0, 1, map[string]composeFlag{"--json": {"json", false}})
		_, asJSON := flags["json"]
		if len(pos) == 0 {
			listComposeProjects(asJSON)
		} else {
			listComposeProject(pos[0], asJSON)
		}

	case "suspend":
		pos, _ := composeArgs(cmd, args, 1, 1, nil)
		if err := group.Suspend(pos[0]); err != nil {
			composeFail(err)
		}
		fmt.Printf("Project '%s' suspended; fork a checkpoint to continue\n", pos[0])

	case "cleanup":
		pos, flags := composeArgs(cmd, args, 1, 1, map[string]composeFlag{"--force": {"force", false}})
		_, force := flags["force"]
		if err := group.Cleanup(pos[0], force); err != nil {
			composeFail(err)
		}
		if waypoint.PreserveSessionOnCleanup {
			fmt.Printf("Project '%s' stopped; the project and its sessions are preserved\n", pos[0])
			return
		}
		fmt.Printf("Project '%s' cleaned up\n", pos[0])

	default:
		composeUsage("")
	}
}

func listComposeProjects(asJSON bool) {
	names, err := group.ListProjects()
	if err != nil {
		composeFail(err)
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
}

func listComposeProject(name string, asJSON bool) {
	listing, err := group.List(name)
	if err != nil {
		composeFail(err)
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
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		composeFail(fmt.Errorf("encoding output: %w", err))
	}
	fmt.Println(string(data))
}
