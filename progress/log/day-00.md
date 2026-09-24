# Day 0 — Thu 24 Sep: lab setup check

**Status:** done. Multipass VM running; `sudo rh run --rm mirror.gcr.io/library/alpine:3.20 echo ok` works. About 30–40 minutes.
**Problems solved today:** `rh: command not found` (clone/build step had not completed); `build constraints exclude all Go files` (no C compiler, so Go disabled cgo; fixed with `build-essential`). The repo now prints a clearer error for the second one.
**Not done:** dashboard (`sudo ./install.sh --lan`) — first thing on Day 1.

## Baseline questions (not counted in averages)

| #   | Question                                 | Gist of answer                                 | Score | Feedback                                                                                                                                     |
| --- | ---------------------------------------- | ---------------------------------------------- | ----- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | What is cgo and why was it off?          | "C compiler for Go; no compiler so turned off" | 1     | cgo is Go's mechanism for including/calling C; it needs gcc to build; Go disables it silently without one, leaving C-only packages empty.    |
| 2   | Why a Linux VM on a Mac?                 | "macOS is a constrained Linux"                 | 0     | macOS runs a different kernel (XNU). Containers are Linux kernel features (namespaces, cgroups, overlayfs); Docker Desktop hides a Linux VM. |
| 3   | What does `cgroup.controllers` tell you? | "list of CPU processes"                        | 0     | It lists resource controllers available (cpu, memory, pids, io…) and shows the host uses cgroup v2.                                          |

Baseline: 2/6 points (17%).

## Tomorrow (Day 1)

1. Open the dashboard (`git pull && sudo ./install.sh --lan`).
2. F1 sections 1–3: processes, fork/exec/wait, system calls, file descriptors. Lab with `strace`.
3. Morning warm-up will revisit today's three topics.
