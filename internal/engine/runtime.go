package engine

import (
	"context"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/cgroups"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/container"
)

// Runtime is everything the engine needs from the node. The production
// implementation is the container manager; tests use a fake. Keeping the
// control loop behind this seam is what lets its trickiest logic (rollouts,
// rollbacks, crash loops, restart recovery) be tested in milliseconds
// without root.
type Runtime interface {
	HasImage(ref string) bool
	PullImage(ctx context.Context, ref string) error
	Create(o container.CreateOptions) (string, error)
	Start(id string) error
	// Restart starts an exited container again and bumps its restart count.
	Restart(id string) error
	Stop(id string, grace time.Duration) error
	Remove(id string) error
	// List is the observed state: every container on the node.
	List() ([]container.Info, error)
	Stats(id string) (cgroups.Stats, error)
	Logs(ctx context.Context, id string, follow bool, tail int, fn func(container.LogLine) error) error
}

// NodeRuntime adapts container.Manager to Runtime.
type NodeRuntime struct{ M *container.Manager }

func (r NodeRuntime) HasImage(ref string) bool {
	_, err := r.M.Images.Get(ref)
	return err == nil
}

func (r NodeRuntime) PullImage(ctx context.Context, ref string) error {
	_, err := r.M.Images.Pull(ctx, ref, nil)
	return err
}

func (r NodeRuntime) Create(o container.CreateOptions) (string, error) {
	c, err := r.M.Create(o)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func (r NodeRuntime) Start(id string) error {
	_, err := r.M.StartDetached(id)
	return err
}

func (r NodeRuntime) Restart(id string) error {
	if err := r.M.BumpRestarts(id); err != nil {
		return err
	}
	_, err := r.M.StartDetached(id)
	return err
}

func (r NodeRuntime) Stop(id string, grace time.Duration) error { return r.M.Stop(id, grace) }
func (r NodeRuntime) Remove(id string) error                    { return r.M.Remove(id, true) }
func (r NodeRuntime) List() ([]container.Info, error)           { return r.M.List() }
func (r NodeRuntime) Stats(id string) (cgroups.Stats, error)    { return r.M.Stats(id) }
func (r NodeRuntime) Logs(ctx context.Context, id string, follow bool, tail int, fn func(container.LogLine) error) error {
	return r.M.ReadLogs(ctx, id, follow, tail, fn)
}
