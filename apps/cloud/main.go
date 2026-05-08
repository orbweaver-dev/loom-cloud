// Command loom-cloud-edge is the loom-cloud edge runtime.
//
// Production builds set up:
//   - The Site Thread for the platform's tenant tracking.
//   - The DockerProvisioner for spinning up tenant containers.
//   - The edge.Router fronting all *.loom.dev traffic.
//   - DNS automation (Cloudflare) for `<slug>.<base>` records.
//   - TLS automation (Let's Encrypt via autocert) for the same.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/orbweaver-dev/loom-cloud/internal/dns"
	"github.com/orbweaver-dev/loom-cloud/internal/edge"
	cloudtls "github.com/orbweaver-dev/loom-cloud/internal/tls"
)

func main() {
	addr := flag.String("addr", ":443", "edge listener address (https)")
	httpAddr := flag.String("http-addr", ":80", "plaintext listener (handles ACME http-01 + redirects to https)")
	baseDomain := flag.String("base-domain", "loom.dev", "parent domain under which tenant subdomains live")
	tlsCacheDir := flag.String("tls-cache", "/var/lib/loom-cloud/certs", "where autocert persists keys + certs")
	tlsEmail := flag.String("tls-email", "", "Let's Encrypt account email (renewal warnings go here)")
	tlsStaging := flag.Bool("tls-staging", false, "use Let's Encrypt staging (no rate limits, certs not browser-trusted)")
	disableTLS := flag.Bool("no-tls", false, "serve plaintext on --addr (dev only)")
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
	// tenants come and go.
	dnsMgr := buildDNSManager(*baseDomain)
	if dnsMgr != nil {
		slog.Info("dns automation enabled", "base_domain", *baseDomain, "edge_ip", dnsMgr.EdgeIP)
	} else {
		slog.Info("dns automation disabled — set CLOUDFLARE_API_TOKEN + CLOUDFLARE_ZONE_ID + LOOM_EDGE_IP to enable")
	}
	_ = dnsMgr // staged for the watcher loop

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

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-stop.Done()
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdown)
	}()

	// TLS automation: build the manager and wire its TLSConfig
	// onto the server. The HTTP-01 challenge listener runs in
	// parallel on --http-addr so autocert can answer ACME
	// validations + redirect plain-HTTP traffic to https.
	if !*disableTLS {
		tlsMgr, err := cloudtls.NewManager(cloudtls.Options{
			CacheDir:   filepath.Clean(*tlsCacheDir),
			Email:      *tlsEmail,
			BaseDomain: *baseDomain,
			Staging:    *tlsStaging,
			AllowSlug:  func(slug string) bool { _, ok, _ := portMap.Lookup(stop, slug); return ok },
		})
		if err != nil {
			slog.Error("tls setup failed", "err", err)
			os.Exit(1)
		}
		srv.TLSConfig = tlsMgr.TLSConfig()
		go func() {
			httpSrv := &http.Server{
				Addr:              *httpAddr,
				Handler:           tlsMgr.HTTPHandler(nil),
				ReadHeaderTimeout: 10 * time.Second,
			}
			slog.Info("acme http-01 listener", "addr", *httpAddr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("http listener failed", "err", err)
			}
		}()
		slog.Info("loom-cloud edge listening (https)", "addr", *addr, "base_domain", *baseDomain, "staging", *tlsStaging)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			slog.Error("listen tls failed", "err", err)
			os.Exit(1)
		}
		return
	}

	slog.Info("loom-cloud edge listening (plaintext, --no-tls)", "addr", *addr, "base_domain", *baseDomain)
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
