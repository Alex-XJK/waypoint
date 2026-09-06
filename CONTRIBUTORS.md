# Contributors

This project thrives thanks to the efforts and expertise of the following people.

## Architect & Project Lead

### Alex Jiakai Xu

- Project founder, principal architect, and active maintainer.
- Continues to lead Waypoint's technical direction, system design, implementation, integration, and release planning.
- Created the initial prototype, core checkpoint manager, CLI, and configuration system.
- Designed and implemented the v0.4.0 in-place filesystem architecture and subsequent OverlayFS checkpoint/restore pipeline.
- Designed the v0.5.0 RPC-style shell session and continues to lead the evolution of the Dockerfile build, runtime, restore-readiness, installation, and usability workflows.
- Proposed the initial direction of the v0.7.0 concurrent fork architecture and guided its design through implementation and release.


## Project Contributors


### Andy Tiancheng Ge

- Fixed Dockerfile image-tag generation for unusual build-context directory names.
- Added CRIU file-lock support for lock-holding processes.
- Added inode reverse-map and deleted-file remapping support for Node.js workloads with inotify watches and unlinked-but-open files.
- Applied built-image `ENV` and `WORKDIR` settings to Dockerfile-based sessions.
- Extended the reach of the concurrent fork model with the `cp` verb for host-to-fork file and directory transfer, and by letting sessions and forks share the host network namespace.
- Designed a user field verification and Bash command pre-validation module to enhance the robustness.


### Rohan Timmaraju

- Key implementer of the v0.7.0 concurrent fork architecture, turning the checkpoint DAG and live-fork design into the shipping implementation.
- Built the fork lifecycle end to end: materialization of live forks from checkpoints, fork snapshot and rebase, `snapshot --park`, and removal of the legacy destructive restore model.
- Made the `main` shell a first-class fork and designed the v2 exec protocol with out-of-band completion signalling and true command exit codes.
- Added `suspend`, tmpfs-backed CRIU images with asynchronous flush, phase-level latency instrumentation, and the identity-checked teardown rework.
- Wrote the architecture and exec-protocol guides, and turned the demo script into an asserting end-to-end test.


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
