# Core Contributors
This project thrives thanks to the efforts and expertise of the following people:

## Architect & Project Lead

### Alex Jiakai Xu

- Project founder and principal architect.
- Created the initial prototype and foundational architecture.
- Designed and implemented the major v0.4.0 architecture shift.
- Invented the v0.5.0 RPC-style shell session.


## Isolation & Security, Shell Session

### Georgios Liargkovas

- Developed the v0.4.0 command injection-style shell session.
- Provided key support and guidance for system isolation and security topics.


## Concurrent Forking Design

### Danielle Gillai

- Led early design and experimentation on concurrent forking support.
- Prototyped filesystem-only and CRIU-ns fork paths, helping clarify the terminal-session challenges that shaped later fork designs.


## Environment Build Workflow

### Tianle Zhou

- Designed the original Buildah-based environment build workflow in the TBench integration, which informed Waypoint's v0.5.0 Dockerfile-based `build` command.


## Usability and Robustness Improvements

### Andy Tiancheng Ge

- Fixed the Dockerfile image-tag sanitization issue for generated or unusual build-context directory names.
- Added CRIU compatibility flags for lock-holding processes and Node.js workloads that use inotify watches or unlinked-but-open files.


## Advisors

### Prof. Kostis Kaffes, Prof. Eugene Wu

- Serve as project advisors.
- Provide expert guidance on systems, architecture, and research directions.


> To contribute, please submit a PR or contact the maintainers. All contributions, large or small, are appreciated!
