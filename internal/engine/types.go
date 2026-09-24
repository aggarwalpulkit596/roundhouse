// Package engine is the container provisioning engine: the control loop that
// turns declared services into running, healthy, routed containers and keeps
// them that way.
//
// The model mirrors how Railway (and most PaaS control planes) present
// things to users:
//
//	Service     a long-lived name with a desired spec ("web", "postgres")
//	Deployment  one immutable version of that spec; every change creates one
//	Instance    one running container (replica) of a deployment
//
// The engine is level-triggered: it never trusts that an action it took
// earlier still holds. Every sync compares the *desired* state (services and
// deployments, persisted in state.json) with the *observed* state (the
// containers that actually exist, discovered by label) and acts on the
// difference. That is what makes it safe to crash and restart at any point.
package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Deployment statuses. The lifecycle is:
//
//	QUEUED → DEPLOYING → ACTIVE → REMOVED        (replaced by a newer deploy)
//	                   ↘ FAILED                  (never became healthy; the
//	                                              previous ACTIVE keeps serving)
//	         ACTIVE → CRASHED                    (exceeded its restart budget)
//	QUEUED → SKIPPED                             (a newer deploy superseded it
//	                                              before it started)
const (
	StatusQueued    = "QUEUED"
	StatusDeploying = "DEPLOYING"
	StatusActive    = "ACTIVE"
	StatusFailed    = "FAILED"
	StatusCrashed   = "CRASHED"
	StatusRemoved   = "REMOVED"
	StatusSkipped   = "SKIPPED"
)

// Restart policies.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// ServiceSpec is what a user declares. It is deliberately close to a
// railway.json / docker-compose service.
type ServiceSpec struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Cmd   []string          `json:"cmd,omitempty"`
	Env   map[string]string `json:"env,omitempty"`

	// Port is the port the application listens on inside the container. It
	// is injected as $PORT, as Railway does.
	Port int `json:"port,omitempty"`
	// PublicPort, if set, exposes the service on the host through the edge
	// proxy. 0 means private networking only (<name>.rh.internal).
	PublicPort int `json:"publicPort,omitempty"`

	Replicas int     `json:"replicas,omitempty"`
	CPU      float64 `json:"cpu,omitempty"`      // cores, e.g. 0.5
	MemoryMB int     `json:"memoryMb,omitempty"` // hard limit
	PidsMax  int     `json:"pidsMax,omitempty"`

	Healthcheck *Healthcheck `json:"healthcheck,omitempty"`

	Restart     string `json:"restart,omitempty"`
	MaxRestarts int    `json:"maxRestarts,omitempty"`

	// Volume gives the service persistent storage. Like Railway volumes, a
	// service with a volume runs exactly one replica: two writers on one
	// filesystem is a database corruption bug waiting to happen.
	Volume *Volume `json:"volume,omitempty"`

	// DrainSeconds is how long replaced instances keep running (out of the
	// load balancer) so in-flight requests can finish.
	DrainSeconds int `json:"drainSeconds,omitempty"`
	// StopGraceSeconds is SIGTERM → SIGKILL.
	StopGraceSeconds int `json:"stopGraceSeconds,omitempty"`
	// DeployTimeoutSeconds bounds how long a deployment may take to become
	// healthy before it is marked FAILED.
	DeployTimeoutSeconds int `json:"deployTimeoutSeconds,omitempty"`
}

// Healthcheck gates rollouts and routing (a readiness check).
type Healthcheck struct {
	Type            string `json:"type,omitempty"` // http (default when Path set), tcp, none
	Path            string `json:"path,omitempty"`
	IntervalSeconds int    `json:"intervalSeconds,omitempty"`
	TimeoutSeconds  int    `json:"timeoutSeconds,omitempty"`
}

// Volume is a named persistent directory.
type Volume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

// Deployment is one immutable version of a service.
type Deployment struct {
	ID        string      `json:"id"`
	Service   string      `json:"service"`
	Revision  int         `json:"revision"`
	Spec      ServiceSpec `json:"spec"`
	Status    string      `json:"status"`
	Reason    string      `json:"reason,omitempty"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
	// StartedAt is when it entered DEPLOYING; the deploy timeout runs from
	// here.
	StartedAt time.Time `json:"startedAt,omitempty"`
	ActiveAt  time.Time `json:"activeAt,omitempty"`
	// DrainUntil is set when an ACTIVE deployment is replaced.
	DrainUntil time.Time `json:"drainUntil,omitempty"`
	// Logs holds the last output lines of a FAILED deployment's instances.
	Logs []string `json:"logs,omitempty"`
}

// Service is the persisted desired state for one service.
type Service struct {
	Name        string        `json:"name"`
	Deployments []*Deployment `json:"deployments"` // oldest first
	Deleted     bool          `json:"deleted,omitempty"`
	NextRev     int           `json:"nextRevision"`
}

// Latest returns the newest deployment.
func (s *Service) Latest() *Deployment {
	if len(s.Deployments) == 0 {
		return nil
	}
	return s.Deployments[len(s.Deployments)-1]
}

// Active returns the deployment currently serving traffic, if any.
func (s *Service) Active() *Deployment {
	for i := len(s.Deployments) - 1; i >= 0; i-- {
		if d := s.Deployments[i]; d.Status == StatusActive {
			return d
		}
	}
	return nil
}

// Target returns the deployment being rolled out, if any.
func (s *Service) Target() *Deployment {
	for i := len(s.Deployments) - 1; i >= 0; i-- {
		d := s.Deployments[i]
		if d.Status == StatusQueued || d.Status == StatusDeploying {
			return d
		}
	}
	return nil
}

// Find returns a deployment by ID or ID prefix.
func (s *Service) Find(id string) *Deployment {
	for _, d := range s.Deployments {
		if d.ID == id || (len(id) >= 4 && strings.HasPrefix(d.ID, id)) {
			return d
		}
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// Normalize validates a spec and fills in defaults.
func (s *ServiceSpec) Normalize() error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("service name %q must be 2-32 chars of a-z, 0-9 and '-' (it becomes a DNS label)", s.Name)
	}
	if s.Image == "" {
		return fmt.Errorf("service %s: image is required", s.Name)
	}
	if s.Replicas == 0 {
		s.Replicas = 1
	}
	if s.Replicas < 0 || s.Replicas > 50 {
		return fmt.Errorf("service %s: replicas must be 1-50", s.Name)
	}
	if s.Volume != nil {
		if s.Replicas != 1 {
			return fmt.Errorf("service %s: a service with a volume runs exactly one replica", s.Name)
		}
		if !nameRE.MatchString(s.Volume.Name) || !strings.HasPrefix(s.Volume.MountPath, "/") {
			return fmt.Errorf("service %s: volume needs a DNS-style name and an absolute mountPath", s.Name)
		}
	}
	if s.Port < 0 || s.Port > 65535 || s.PublicPort < 0 || s.PublicPort > 65535 {
		return fmt.Errorf("service %s: invalid port", s.Name)
	}
	if s.PublicPort > 0 && s.Port == 0 {
		return fmt.Errorf("service %s: publicPort needs port (where the app listens)", s.Name)
	}
	switch s.Restart {
	case "":
		s.Restart = RestartOnFailure
	case RestartAlways, RestartOnFailure, RestartNever:
	default:
		return fmt.Errorf("service %s: restart must be always, on-failure or never", s.Name)
	}
	if s.MaxRestarts == 0 {
		s.MaxRestarts = 10
	}
	if s.DrainSeconds == 0 {
		s.DrainSeconds = 5
	}
	if s.StopGraceSeconds == 0 {
		s.StopGraceSeconds = 10
	}
	if s.DeployTimeoutSeconds == 0 {
		s.DeployTimeoutSeconds = 120
	}
	if h := s.Healthcheck; h != nil {
		if h.Type == "" {
			h.Type = "http"
			if h.Path == "" {
				h.Type = "tcp"
			}
		}
		if h.Type != "http" && h.Type != "tcp" && h.Type != "none" {
			return fmt.Errorf("service %s: healthcheck type must be http, tcp or none", s.Name)
		}
		if h.Type != "none" && s.Port == 0 {
			return fmt.Errorf("service %s: a healthcheck needs port", s.Name)
		}
		if h.Type == "http" && !strings.HasPrefix(h.Path, "/") {
			h.Path = "/" + h.Path
		}
		if h.IntervalSeconds == 0 {
			h.IntervalSeconds = 2
		}
		if h.TimeoutSeconds == 0 {
			h.TimeoutSeconds = 2
		}
	}
	for k := range s.Env {
		if k == "" || strings.ContainsAny(k, "= ") {
			return fmt.Errorf("service %s: invalid env name %q", s.Name, k)
		}
	}
	return nil
}

// sortedEnv renders env deterministically.
func sortedEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
