# Installing Waypoint

Waypoint is a Go CLI, but it is not a pure user-space program. It directly
uses Linux process management and filesystem features, so installation has two
parts:

1. Build and install the Waypoint binaries.
2. Make sure the host has the system tools and kernel support Waypoint calls.

The repository supports both a no-extra-tool shell entry point and a conventional
Makefile:

```bash
./setup build
./setup test
sudo ./setup install
sudo ./setup check
```

or:

```bash
make build
make test
sudo make install
sudo make check
```

## Quick Start On Ubuntu/Debian

From the Waypoint checkout:

```bash
sudo ./setup deps-ubuntu
./setup build
sudo ./setup install
sudo ./setup check
```

`deps-ubuntu` intentionally changes system state. It installs common host packages,
enables Ubuntu `universe`, falls back to `ppa:criu/ppa` when CRIU is unavailable
from the configured apt sources, and installs the Go version from `go.mod` into
`/usr/local/go` when `go` is missing or too old. The CRIU it installs is the
distribution package; since v0.8.0 Waypoint needs our own CRIU build installed
beside it, see [CRIU Build](#criu-build).

## CRIU Build

Since v0.8.0, Waypoint requires the `waypoint` branch of
[our CRIU fork](https://github.com/Alex-XJK/criu/tree/waypoint), version
4.2.1.20261101 or later. It adds what Waypoint needs on top of CRIU 4.2: a restore
that leaves the tree ready for an incremental dump (`restore --next-images-dir`), a
dump that links its parent past that image set, `ns_last_pid` restored with each
pid namespace, and a fix for a crash when restoring socketpairs whose holders have
inverted pids. Keep the distribution package for CRIU's runtime libraries and the
`crit` command, and install our `criu` beside it; the build dependencies are listed
at <https://criu.org/Installation>:

```bash
git clone -b waypoint https://github.com/Alex-XJK/criu.git
cd criu
make -j"$(nproc)"
sudo make install-criu        # /usr/local/sbin/criu
criu --version                # Version: 4.2.1.20261101 or later
```

`/usr/local/sbin` precedes `/usr/sbin` in root's default `PATH`, so `sudo waypoint`
and `sudo ./setup check` pick up the new build; `sudo waypoint info` reports the
version found. The stock CRIU package is not fully supported: Waypoint runs on it
with full dumps only, `incremental_dumps` fails at the first fork, and a restored
tree hands out pids below the ones it already holds, which breaks the next restore
of a tree holding a socketpair across such pids.

## What Gets Installed

By default, `sudo ./setup install` installs:

- `waypoint` to `/usr/local/bin/waypoint`
- `bash_init` to `/usr/local/libexec/waypoint/bash_init`
- Bash completion to
  `/usr/local/share/bash-completion/completions/waypoint`
- `/etc/waypoint/config.json` with `bash_init_src` pointing at the installed
  helper

You can override install paths:

```bash
PREFIX=/opt/waypoint sudo -E ./setup install
```

or more specifically:

```bash
BINDIR=/usr/bin LIBEXECDIR=/usr/libexec/waypoint \
  BASH_COMPLETION_DIR=/usr/share/bash-completion/completions \
  sudo -E ./setup install
```

If `/etc/waypoint/config.json` already exists, the installer preserves it. Use
`FORCE_CONFIG=1 sudo -E ./setup install` to replace it with the default config.

## Bash Completion

The installed completion is discovered automatically by the standard
`bash-completion` package. Start a new Bash session after installation, then
complete commands and host-directory arguments with Tab:

```bash
waypoint sna<Tab>
waypoint init /path/to/proj<Tab>
```

Ubuntu's standard `sudo` completion delegates to command-specific helpers, so
completion also works after `sudo waypoint`.

The helper completes public subcommands and aliases, supported flags, directory
arguments for `init` and `build`, and local files and directories for `cp`. It
uses only the existing `waypoint list` and `waypoint list <session>` commands,
with simple text filtering, to complete session, checkpoint, and fork IDs. For
`cp`, fork IDs are offered as `fork-id:/` prefixes. It recognizes `snapshot`'s
`--park` flag anywhere among its arguments and offers the required `--`
separator for `exec`.

New checkpoint and fork IDs, fork counts, guest commands, and guest file paths
remain user-supplied. The current CLI has no guest-filesystem listing command;
completion does not execute commands inside a fork. Live-state completion is
best effort: failed or unrecognized `list` output produces no candidates and
never interrupts the command line.

For development, print or source the repository copy without installing it:

```bash
make completion
source <(./setup completion)
```

Run the focused completion checks with `bash scripts/test-bash-completion.sh`.
They also run as part of `make test` and require no live Waypoint sessions.

## Host Dependencies

Required for normal operation:

- Linux with root privileges
- OverlayFS support
- CRIU, including the `criu` and `crit` commands: the `waypoint` branch of our
  CRIU fork, 4.2.1.20261101 or later (see [CRIU Build](#criu-build))
- `cp` and `uname` (coreutils)
- `/bin/bash` inside the workspace/rootfs when using `--shell`

Required for building from source:

- Go 1.25 or newer. `sudo ./setup deps-ubuntu` installs the version listed in
  `go.mod` when `go` is missing or too old.

Optional for `waypoint build`:

- `buildah`

Optional for the `private` network mode:

- `pasta`, from the `passt` package

Run `sudo ./setup check`, `sudo make check`, or `sudo make doctor` to inspect the current host.
The check requires root so it can run CRIU the same way Waypoint does. Plain `criu check` is a hard gate; `criu check --all` is reported as an advisory warning because optional missing kernel features may not affect every workload. The check is diagnostic only; it does not add a Waypoint CLI sub-command.

## Development Workflow

```bash
./setup build
./setup test
./setup clean
```

The build outputs live in `bin/`. Historical root-level binaries such as
`./waypoint` and `./bash_init` are ignored and cleaned by `./setup clean`.
