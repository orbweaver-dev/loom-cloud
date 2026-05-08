// Command loom-cloud-edge is the v0.0.1 edge runtime.
//
// Production builds set up:
//   - The Site Thread for the platform's tenant tracking.
//   - The DockerProvisioner for spinning up tenant containers.
//   - The edge.Router fronting all *.loom.dev traffic.
//
// v0.0.1 ships the wire-up; the actual tenant deployment loop
// (a watcher that reacts to Site row INSERTs by calling
// Provision, then publishes the host port to the edge router's
// PortMap) is a follow-up.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/orbweaver-dev/loom-cloud/internal/dns"
	"github.com/orbweaver-dev/loom-cloud/internal/edge"
)

func main() {
	addr := flag.String("addr", ":443", "edge listener address")
	baseDomain := flag.String("base-domain", "loom.dev", "parent domain under which tenant subdomains live")
	flag.Parse()

	// Single-host port map for the v0.0.1 dev / smoke setup.
	// Production replaces this with a SQL-backed implementation
	// the Provisioner writes to.
	portMap := edge.NewMemoryPortMap()

	// Demo entry — register a localhost tenant so the smoke
	// test of `curl -H "Host: demo.loom.dev" http://127.0.0.1:8080/`
	// has somewhere to land.
	if v := os.Getenv("LOOM_CLOUD_DEMO_PORT"); v != "" {
		var port int
		_, _ = fmt.Sscanf(v, "%d", &port)
		if port > 0 {
			portMap.Set("demo", port)
			slog.Info("registered demo tenant", "slug", "demo", "port", port)
		}
	}

	// DNS automation: when CLOUDFLARE_API_TOKEN + CLOUDFLARE_ZONE_ID
	// + LOOM_EDGE_IP are all set, build a Manager so the
	// provisioner watcher can call EnsureSlug / RemoveSlug as
	// tenants come and go. When env is missing we run without
	// DNS — fine for local dev where /etc/hosts handles it.
	dnsMgr := buildDNSManager(*baseDomain)
	if dnsMgr != nil {
		slog.Info("dns automation enabled", "base_domain", *baseDomain, "edge_ip", dnsMgr.EdgeIP)
	} else {
		slog.Info("dns automation disabled — set CLOUDFLARE_API_TOKEN + CLOUDFLARE_ZONE_ID + LOOM_EDGE_IP to enable")
	}
	_ = dnsMgr // wired up for the watcher loop; not used by the v0.0.1 main directly

	router := &edge.Router{PortMap: portMap}
	handler, err := router.Handler(*baseDomain)
	if err != nil {
		slog.Error("edge router setup failed", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("loom-cloud edge listening", "addr", *addr, "base_domain", *baseDomain)

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-stop.Done()
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen failed", "err", err)
		os.Exit(1)
	}
}

// buildDNSManager assembles a dns.Manager from environment
// variables. Returns nil when any required value is missing —
// callers treat nil as "DNS automation disabled" and fall back
// to manual DNS / /etc/hosts.
func buildDNSManager(baseDomain string) *dns.Manager {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	zoneID := os.Getenv("CLOUDFLARE_ZONE_ID")
	edgeIP := os.Getenv("LOOM_EDGE_IP")
	if token == "" || zoneID == "" || edgeIP == "" {
		return nil
	}
	return &dns.Manager{
		Provider:   dns.NewCloudflareProvider(token, zoneID),
		BaseDomain: baseDomain,
		EdgeIP:     edgeIP,
	}
}
