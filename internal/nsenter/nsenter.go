// Package nsenter joins the namespaces of a running container before the Go
// runtime starts.
//
// Why C? setns(CLONE_NEWNS) fails with EINVAL in a multi-threaded process,
// and by the time main() runs, the Go runtime already has several threads.
// A C constructor runs before the Go runtime initializes, while the process
// is still single-threaded. runc solves the same problem the same way
// (libcontainer/nsenter/nsexec.c).
//
// It also forks once after setns, because joining a PID namespace only
// applies to children and Linux will not create threads until the process
// is actually inside it. runc's nsexec.c does the same double dance.
//
// Importing this package for side effects is enough. The constructor does
// nothing unless _RH_NSENTER_PID is set in the environment.
package nsenter

/*
#cgo CFLAGS: -Wall
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

static pid_t rh_child;

static void rh_forward(int sig) {
	if (rh_child > 0)
		kill(rh_child, sig);
}

__attribute__((constructor)) static void rh_nsenter(void) {
	const char *pid = getenv("_RH_NSENTER_PID");
	if (pid == NULL || *pid == '\0')
		return;

	// Join the container's cgroup(s) first, while /sys/fs/cgroup is still
	// the host's: _RH_NSENTER_CGROUPS is a ':'-separated list of
	// cgroup.procs files. Every thread and child we create inherits it.
	const char *cgs = getenv("_RH_NSENTER_CGROUPS");
	if (cgs != NULL && *cgs != '\0') {
		char *list = strdup(cgs), *save = NULL;
		char buf[32];
		int n = snprintf(buf, sizeof(buf), "%d", getpid());
		for (char *p = strtok_r(list, ":", &save); p; p = strtok_r(NULL, ":", &save)) {
			int fd = open(p, O_WRONLY | O_CLOEXEC);
			if (fd < 0 || write(fd, buf, n) != n) {
				fprintf(stderr, "nsenter: join cgroup %s: %s\n", p, strerror(errno));
				exit(125);
			}
			close(fd);
		}
		free(list);
		unsetenv("_RH_NSENTER_CGROUPS");
	}

	// Order matters: open every fd first (the mount namespace changes what
	// /proc means), and join the mount namespace last.
	static const struct { const char *name; int type; } ns[] = {
		{ "cgroup", CLONE_NEWCGROUP },
		{ "ipc",    CLONE_NEWIPC },
		{ "uts",    CLONE_NEWUTS },
		{ "net",    CLONE_NEWNET },
		{ "pid",    CLONE_NEWPID },  // applies to children we fork later
		{ "mnt",    CLONE_NEWNS },
	};
	int n = sizeof(ns) / sizeof(ns[0]);
	int fds[sizeof(ns) / sizeof(ns[0])];
	char path[64];
	for (int i = 0; i < n; i++) {
		snprintf(path, sizeof(path), "/proc/%s/ns/%s", pid, ns[i].name);
		fds[i] = open(path, O_RDONLY | O_CLOEXEC);
		if (fds[i] < 0) {
			fprintf(stderr, "nsenter: open %s: %s\n", path, strerror(errno));
			exit(125);
		}
	}
	for (int i = 0; i < n; i++) {
		if (setns(fds[i], ns[i].type) < 0) {
			fprintf(stderr, "nsenter: setns %s: %s\n", ns[i].name, strerror(errno));
			exit(125);
		}
		close(fds[i]);
	}
	// Joining a mount namespace resets root and cwd to that namespace's
	// root, which after pivot_root is the container's filesystem.
	unsetenv("_RH_NSENTER_PID");

	// setns(CLONE_NEWPID) only changes the namespace of future children, and
	// the kernel refuses to create *threads* (CLONE_THREAD) while the two
	// differ. The Go runtime needs threads, so fork once here: the child is
	// a real member of the container's PID namespace and goes on to run Go.
	// The parent stays behind, relays signals, and mirrors the exit status.
	rh_child = fork();
	if (rh_child < 0) {
		perror("nsenter: fork");
		exit(125);
	}
	if (rh_child == 0)
		return;
	int sigs[] = { SIGINT, SIGTERM, SIGHUP, SIGQUIT, SIGUSR1, SIGUSR2, SIGWINCH };
	for (unsigned i = 0; i < sizeof(sigs) / sizeof(sigs[0]); i++)
		signal(sigs[i], rh_forward);
	int status;
	while (waitpid(rh_child, &status, 0) < 0) {
		if (errno != EINTR)
			exit(125);
	}
	if (WIFSIGNALED(status))
		exit(128 + WTERMSIG(status));
	exit(WEXITSTATUS(status));
}
*/
import "C"
