package container

import (
	"fmt"
	"io"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/runtime"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/spec"
)

// ExecOptions describes a process to add to a running container.
type ExecOptions struct {
	Args []string
	Env  []string
	User string
	Cwd  string
}

// Exec runs a process inside a running container and returns its exit
// status. The process joins the container's namespaces and cgroup, so it is
// limited and metered like the rest of the container.
func (m *Manager) Exec(ref string, o ExecOptions, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c, err := m.Get(ref)
	if err != nil {
		return -1, err
	}
	if c.State.Status != StatusRunning {
		return -1, fmt.Errorf("container %s is not running", c.Name)
	}
	if len(o.Args) == 0 {
		return -1, fmt.Errorf("exec needs a command")
	}
	p := spec.Process{
		Args:            o.Args,
		Env:             MergeEnv(c.Spec.Process.Env, o.Env),
		Cwd:             c.Spec.Process.Cwd,
		UID:             c.Spec.Process.UID,
		GID:             c.Spec.Process.GID,
		AdditionalGIDs:  c.Spec.Process.AdditionalGIDs,
		Capabilities:    c.Spec.Process.Capabilities,
		NoNewPrivileges: true,
	}
	if o.Cwd != "" {
		p.Cwd = o.Cwd
	}
	if o.User != "" {
		uid, gid, groups, err := ResolveUser(c.Spec.RootFS, o.User)
		if err != nil {
			return -1, err
		}
		p.UID, p.GID, p.AdditionalGIDs = uid, gid, groups
	}
	return runtime.Exec(c.State.Pid, runtime.ExecConfig{
		Process:      p,
		CgroupParent: c.Spec.CgroupParent,
		ContainerID:  c.ID,
	}, stdin, stdout, stderr)
}
