# Contributors

This project thrives thanks to the efforts and expertise of the following people.

## Architect & Project Lead

### Alex Jiakai Xu

- Project founder, principal architect, and active maintainer.
- Continues to lead Waypoint's technical direction, system design, implementation, integration, and release planning.
- Created the initial prototype, core checkpoint manager, CLI, and configuration system.
- Designed and implemented the v0.4.0 in-place filesystem architecture and subsequent OverlayFS checkpoint/restore pipeline.
- Designed the v0.5.0 RPC-style shell session and continues to lead the evolution of the Dockerfile build, runtime, restore-readiness, installation, and usability workflows.


## Project Contributors


### Andy Tiancheng Ge

- Fixed Dockerfile image-tag generation for unusual build-context directory names.
- Added CRIU file-lock support for lock-holding processes.
- Added inode reverse-map and deleted-file remapping support for Node.js workloads with inotify watches and unlinked-but-open files.
- Applied built-image `ENV` and `WORKDIR` settings to Dockerfile-based sessions.


### Danielle Gillai

- Led early design and experimentation on concurrent forking support.
- Prototyped filesystem-only and CRIU-ns fork paths and refactored early restore-management code, helping identify the terminal-session challenges that shaped later fork designs.


### Georgios Liargkovas

- Designed and implemented the v0.4.0 sandbox mode and contributed filesystem checkpoint/restore optimizations.
- Developed the command-injection-style shell session that preceded the current RPC-style terminal design.
- Provided guidance on system isolation and security.


### Tianle Zhou

- Designed the original Buildah-based environment build workflow in the TBench integration, which informed Waypoint's v0.5.0 Dockerfile-based `build` command.
- Contributed early runtime-filesystem experiments and shell path-resolution fixes.



## Advisors

### Prof. Kostis Kaffes, Prof. Eugene Wu

- Serve as project advisors.
- Provide expert guidance on systems, architecture, and research directions.


> To contribute, please submit a PR or contact the maintainers. All contributions, large or small, are appreciated!
