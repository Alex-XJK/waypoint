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
  scraping, echo stripping, or prompt heuristics. The handshake also runs
  `set +H`: history expansion is on by default in an interactive bash and off
  under `bash -c`, and callers write commands for the latter (`echo "hi!there"`
  must print, not fail with "event not found").
- The completion line is preceded by a blank line. A payload ending in `\` is
  a line continuation; without the separator the completion line would be
  joined onto the command as its arguments. A blank line is a no-op to bash
  and leaves `$?` untouched.
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
  the completion line): the `waypoint` client refuses such payloads up front
  by parsing them with the host's `bash -n` (no execution), returning bash's
  diagnostic with exit status 2 and never taking the fork lock. Should one
  reach the server anyway (another client, or a host without bash), nothing
  completes until the client disconnects or the backstop fires; the Ctrl-C
  sent then clears bash's continuation state.
- **Shell death**: a liveness check turns "completion will never arrive"
  into an immediate `dead` response instead of a hang.
- **Memory bounds**: request headers are limited to 32 bytes, command payloads
  to 1 MiB, and retained command output to 16 MiB. The PTY drainer always
  consumes excess bytes so a noisy child cannot block the supervisor.

## Syntax precheck

Before taking the fork lock, the client hands the payload to the host's bash
with `-n` (`set -n`, "read commands but do not execute them" — documented
under the `set` builtin, which is where bash's single-letter options live).
The script goes in on **stdin**, not as `-c` text and not as a file name, so
a payload can be as large as the 1 MiB request limit allows without meeting
the 128 KiB per-argument ceiling:

```text
printf '%s' "$payload" | bash -n
```

`-n` runs the parser and nothing else: no command is forked, no path is
looked up, no expansion happens. `ls -al lalala` passes whether or not
`lalala` exists; `rm -rf /` passes and does nothing. The question it answers
is only "is this a complete bash input?" — the fork's own shell answers
everything else.

Two parser outcomes matter, and they are reported differently:

- An unterminated quote, an open `if`/`for`/`case`, a stray `)`: bash prints
  a diagnostic and exits **2**. The client returns that diagnostic (minus the
  `bash: ` prefix) as `ErrCommandSyntax`; the CLI exits 2, like `bash -c`.
- An unterminated here-document: bash prints only a **warning** and exits
  **0** —

  ```text
  bash: line 4: warning: here-document at line 1 delimited by end-of-file (wanted `EOF')
  ```

  — because at end of a script that is a recoverable condition. In a fork it
  is not: everything after the `<<EOF` line, the completion line included,
  is swallowed as heredoc body and the command waits for a delimiter that
  never arrives. The client therefore also refuses any payload whose
  diagnostics contain `delimited by end-of-file`, regardless of exit status.

The heredoc case is the one callers actually hit, and usually through
indentation: a terminator with leading whitespace is data, not a delimiter.

```bash
if true; then
    cat <<EOF
    hello
    EOF          # <- four spaces: not a terminator; the heredoc is still open
fi
```

bash wants the bare word at column 0. The only indented form is `<<-EOF`,
and it strips **tabs** only, never spaces. Agents that indent a heredoc body
to match the surrounding block, then indent the closing `EOF` to match the
body, produce exactly this — the precheck turns what would be a silent hang
into an immediate `warning: here-document … delimited by end-of-file`.

To see the difference in a terminal:

```bash
printf 'cat <<EOF\nhello\nEOF\necho after\n'   | bash -n; echo "exit=$?"   # 0, silent
printf 'cat <<EOF\nhello\necho after\n'         | bash -n; echo "exit=$?"   # 0, but warns
printf 'cat <<EOF\nhello\n  EOF\necho after\n' | bash -n; echo "exit=$?"   # 0, but warns
printf 'echo "unterminated\n'                   | bash -n; echo "exit=$?"   # 2
```

What the check cannot see: a payload ending in a backslash is *complete*
input (the continuation simply ends), so it passes; the server-side blank
line in the framing is what keeps it from joining the completion line.
And the host's bash is not the fork's — a construct the fork's older bash
rejects would pass here and fail there, at run time, as it would under
`bash -c`.

## Known limitations (by design, for now)

- stdout and stderr are merged: both are the same PTY.
- Output produced by background jobs *between* commands is discarded;
  during a command it is captured as that command's output.
- No incremental streaming; up to 16 MiB of output is delivered when the
  command completes.
- The payload is one bash input string, not an argv. The CLI enforces this:
  `exec <session> <fork> --` takes exactly one argument and refuses more, so
  quoting inside that string is the caller's responsibility and nothing is
  ever re-joined or re-quoted on the way in.
- Fully interactive programs (REPLs, pagers) block the exec until the
  client disconnects; the protocol is command/response.
