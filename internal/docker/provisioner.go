// Package docker is loom-cloud's reference Provisioner: spins
// up tenant deployments as Docker containers on the local host.
// Production deployments swap this for a K8s / Fly / Render
// equivalent that satisfies the same hosting.Provisioner
// interface.
//
// The implementation here is intentionally minimal — shells out
// to the `docker` CLI rather than using the Docker Engine API
// client. That keeps loom-cloud's go.mod free of the docker
// SDK's heavyweight dependency tree, at the cost of needing
// `docker` on the host PATH. Production setups should switch
// to the SDK or to a remote engine.
package docker

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/orbweaver-dev/loom/pkg/hosting"
)

// Provisioner is a hosting.Provisioner backed by the local
// docker daemon. Construct with New; safe for concurrent use
// (each method shells out independently).
type Provisioner struct {
	// HealthCheckTimeout bounds how long Provision waits for
	// the container's startup probe to return Running before
	// reporting Failed. Default 60s.
	HealthCheckTimeout time.Duration
	// HealthCheckInterval is the poll interval for the health
	// check. Default 2s.
	HealthCheckInterval time.Duration
	// HostPort is the port the tenant binary listens on inside
	// the container. The provisioner publishes -p HOSTPORT:CTPORT
	// to the host. Default 8080.
	HostPort int
}

// New constructs a Provisioner with defaults applied.
func New() *Provisioner {
	return &Provisioner{
		HealthCheckTimeout:  60 * time.Second,
		HealthCheckInterval: 2 * time.Second,
		HostPort:            8080,
	}
}

// Provision implements hosting.Provisioner. Pulls the image, starts
// the container with the site's env vars, waits for the health
// check, sets site.Status to Running on success.
func (p *Provisioner) Provision(ctx context.Context, site *hosting.Site) error {
	site.Status = hosting.SiteProvisioning
	site.LastError = ""

	// Pull (no-op if already cached).
	if out, err := runCmd(ctx, "docker", "pull", site.Image); err != nil {
		site.Status = hosting.SiteFailed
		site.LastError = fmt.Sprintf("pull: %s", trim(out))
		return err
	}

	// Stop+remove any existing container with the same name
	// (idempotent re-provision).
	name := containerName(site)
	_, _ = runCmd(ctx, "docker", "rm", "-f", name)

	args := []string{"run", "-d", "--name", name,
		"-p", fmt.Sprintf("%d:%d", p.HostPort, p.HostPort)}
	for k, v := range site.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, site.Image)

	out, err := runCmd(ctx, "docker", args...)
	if err != nil {
		site.Status = hosting.SiteFailed
		site.LastError = fmt.Sprintf("run: %s", trim(out))
		return err
	}
	// (Slug stays as-is; the docker container is named after it
	// via containerName above and the host port is published
	// against the slug in the caller's port map.)

	// Health-check loop.
	timeout := p.HealthCheckTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	interval := p.HealthCheckInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			site.Status = hosting.SiteFailed
			site.LastError = "context cancelled during health check"
			return ctx.Err()
		case <-time.After(interval):
		}
		if running, _ := isRunning(ctx, name); running {
			site.Status = hosting.SiteRunning
			site.LastDeployedAt = time.Now()
			return nil
		}
	}

	site.Status = hosting.SiteFailed
	site.LastError = fmt.Sprintf("health check timed out after %s", timeout)
	return fmt.Errorf("docker: health check timed out for site %s", site.Slug)
}

// Deprovision implements hosting.Provisioner. Stops and removes
// the container.
func (p *Provisioner) Deprovision(ctx context.Context, site *hosting.Site) error {
	site.Status = hosting.SiteDeprovisioning
	name := containerName(site)
	if out, err := runCmd(ctx, "docker", "rm", "-f", name); err != nil {
		// Already-gone is fine.
		if !strings.Contains(string(out), "No such container") {
			site.Status = hosting.SiteFailed
			site.LastError = fmt.Sprintf("rm: %s", trim(out))
			return err
		}
	}
	site.Status = hosting.SiteStopped
	return nil
}

// Status implements hosting.Provisioner. Asks docker whether the
// container is running.
func (p *Provisioner) Status(ctx context.Context, site *hosting.Site) (hosting.SiteStatus, error) {
	running, err := isRunning(ctx, containerName(site))
	if err != nil {
		return site.Status, err
	}
	if running {
		return hosting.SiteRunning, nil
	}
	return hosting.SiteStopped, nil
}

// containerName returns the docker container name for a site —
// "loom-<slug>". Stable so re-provision can find the previous
// instance to remove.
func containerName(site *hosting.Site) string {
	return "loom-" + site.Slug
}

// isRunning reports whether docker shows the named container
// as running.
func isRunning(ctx context.Context, name string) (bool, error) {
	out, err := runCmd(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, nil // assume "not running" when inspect fails
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// runCmd is the shell-out helper. Returns combined stderr+stdout
// so error reporting includes whatever docker said.
func runCmd(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, cmd, args...)
	return c.CombinedOutput()
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
