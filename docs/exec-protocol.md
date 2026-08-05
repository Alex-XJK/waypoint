# Waypoint Exec Protocol (v2)

How `waypoint exec` talks to the persistent shell inside a fork.

## Architecture

Each fork runs a checkpointed process tree: `bash_init` (PID 1 of the fork's
namespaces) supervising one long-lived interactive bash on a PTY. Commands
are injected as PTY input, so the shell accumulates real state between
commands — cwd, variables, functions, background jobs, local servers — and
all of it survives checkpoint/restore/fork because CRIU checkpoints the
whole tree.

## Wire protocol

Over the fork's Unix socket (one connection per command):

```text
request:  <decimal payload length>\n<payload bytes>
response: WP2 <status> <exit-code>\n<raw output until connection close>
```

Statuses:

- `ok` — the command completed; exit-code is the command's `$?`.
- `timeout` — the server-side backstop (24h) fired; exit-code is 124.
- `dead` — the shell process is gone (the command ran `exit`, or bash
  crashed). The fork is unusable; `bash_init` exits shortly after, removing
  its socket, so later execs fail at dial time.
- `request_too_large` — the request header is malformed or the payload exceeds
  1 MiB; exit-code is 125 and the command is not sent to bash.
- `output_limit` — command output exceeded 16 MiB; the server interrupts the
  command, keeps draining the PTY so the shell remains synchronized, and
  returns the retained prefix with exit-code 125.

Clients must treat a response without the `WP2` header as v1 output from a
bash_init checkpointed before this protocol existed: the whole stream is
output and the exit code is unknown (reported as 0).

## Completion signalling

Completion and exit codes travel out-of-band. bash_init appends to every
payload:

```bash
builtin printf '%s %s\n' '<nonce>' "$?" > /.waypoint/exec.done
```

`/.waypoint/exec.done` is a FIFO in the session root. `bash_init` holds a
non-blocking read end (plus a keeper write end so reads never EOF) and
matches lines by nonce. Because the completion is a builtin with a
redirection, no helper process is spawned, nothing is left in user
processes' fd tables, and the FIFO fds checkpoint like any other.

Consequences:

- PTY echo is disabled and `PS1`/`PS2`/`PROMPT_COMMAND` are blanked during a
  startup handshake, so the PTY carries *only* program output — no marker
  scraping, echo stripping, or prompt heuristics.
- The startup handshake runs before the socket is created, so the socket's
  existence proves the shell executes commands (not just that bash_init is
  alive).
- Output is returned raw except that PTY `\r\n` translation is undone.
  ANSI escapes pass through.

## Failure handling

- **Client disconnect** (agent kills a laggard command): the server sends
  Ctrl-C to the PTY, SIGTERMs (then SIGKILLs) the foreground process group,
  and waits briefly for the completion line so the next command starts in
  sync.
- **Parser desync** (a payload with an unterminated quote/heredoc swallows
  the completion line): nothing completes until the client disconnects or
  the backstop fires; the Ctrl-C sent then clears bash's continuation state.
- **Shell death**: a liveness check turns "completion will never arrive"
  into an immediate `dead` response instead of a hang.
- **Memory bounds**: request headers are limited to 32 bytes, command payloads
  to 1 MiB, and retained command output to 16 MiB. The PTY drainer always
  consumes excess bytes so a noisy child cannot block the supervisor.

## Known limitations (by design, for now)

- stdout and stderr are merged: both are the same PTY.
- Output produced by background jobs *between* commands is discarded;
  during a command it is captured as that command's output.
- No incremental streaming; up to 16 MiB of output is delivered when the
  command completes.
- The payload is one bash input string, not an argv — quoting is the
  caller's responsibility.
- Fully interactive programs (REPLs, pagers) block the exec until the
  client disconnects; the protocol is command/response.
