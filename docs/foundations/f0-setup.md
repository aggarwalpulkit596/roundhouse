# F0: Set up your lab machine

> **Goal:** a Linux machine where you are root, with Go, Docker and Roundhouse running, in under an hour. Everything else in this curriculum happens here.

Containers are a Linux kernel feature. macOS and Windows run Docker inside a hidden Linux VM, which hides exactly the things you are here to see. So you need your own Linux environment, where breaking things costs nothing.

## Pick one

| Your computer             | Easiest option                                                                                                    | Notes                                                                                                         |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| macOS (Apple Silicon)     | [Multipass](https://canonical.com/multipass): `multipass launch 24.04 --name lab --cpus 2 --memory 4G --disk 30G` | ARM64 Ubuntu. Everything in this curriculum works on arm64; image pulls pick the arm64 variant automatically. |
| macOS (Intel)             | Multipass, same command                                                                                           |                                                                                                               |
| Windows                   | [WSL2](https://learn.microsoft.com/windows/wsl/install) with Ubuntu 24.04                                         | Enable systemd in `/etc/wsl.conf` (`[boot] systemd=true`). WSL2 is a real Linux kernel.                       |
| Linux                     | Use it directly, or a VM (Multipass works on Linux too) so experiments cannot hurt your main system               |                                                                                                               |
| Any, with a cloud account | A small VM: 2 vCPU, 4 GB RAM, Ubuntu 24.04, 30 GB disk (a few dollars a month; stop it when not studying)         | Closest to a real server. Good practice with SSH.                                                             |

Aim for 2 vCPUs, 4 GB of RAM and 30 GB of disk. Less works, but builds get slow.

## Install the tools

On the Ubuntu 24.04 machine:

```sh
sudo apt-get update
sudo apt-get install -y build-essential git curl jq tree htop strace iproute2 iptables \
  bridge-utils conntrack dnsutils netcat-openbsd tcpdump libcap2-bin util-linux psmisc

# Go (check https://go.dev/dl for the latest 1.24+ version and your architecture)
ARCH=$(dpkg --print-architecture)   # amd64 or arm64
curl -fsSL "https://go.dev/dl/go1.24.7.linux-${ARCH}.tar.gz" | sudo tar -C /usr/local -xz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc && source ~/.bashrc
go version

# Docker (to compare Roundhouse with the real thing)
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"     # log out and back in for this to apply
docker run --rm hello-world

# Roundhouse (while the repository is private, clone with SSH or `gh repo clone`)
git clone https://github.com/aggarwalpulkit596/roundhouse && cd roundhouse
go build -o rh ./cmd/rh && sudo install rh /usr/local/bin/rh
sudo rh run --rm mirror.gcr.io/library/alpine:3.20 echo "hello from a container you built"
```

If the last line prints its message, you are ready.

## Habits that make the rest easier

- **Keep a lab notebook.** A markdown file or a paper notebook. For every lab write what you ran, what you expected, and what actually happened. The gap between the last two is where you learn. It is also your interview material.
- **Snapshot before you break things.** `multipass snapshot lab` (or your VM's equivalent). Restoring takes seconds; reinstalling takes an hour.
- **Use two terminals.** Run things in one and watch them (`htop`, `sudo tcpdump`, `watch`) in the other.
- **Read error messages all the way through** before searching for them. Most of them tell you what is wrong.
- **Type the commands, don't paste them** (at least the first time). You remember what you typed.

## Check yourself

1. `uname -a`: which kernel version and architecture are you on?
2. `cat /sys/fs/cgroup/cgroup.controllers`: does it list `memory`? (If so, your machine uses cgroup v2.)
3. `sudo rh run --rm mirror.gcr.io/library/alpine:3.20 ps`: why does the container see only one or two processes?

You can answer question 3 properly after [module 1](../01-namespaces.md). Write your guess in the notebook now and check it later.
