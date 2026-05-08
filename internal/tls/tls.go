// Package tls owns the per-tenant TLS lifecycle for loom-cloud.
//
// Built on golang.org/x/crypto/acme/autocert. The Manager pulls
// certificates from Let's Encrypt on first request to a hostname
// it's authorised to serve, caches them on disk, and renews 30
// days before expiry. The HostPolicy bridges to the same DNS
// manager that creates A records — only hostnames whose slug we
// know about get certificates, so a stranger pointing
// stranger.loom.dev at our IP can't trick us into provisioning a
// cert for them.
//
// Production wire-up (apps/cloud/main.go):
//
//	tlsMgr := tls.NewManager(tls.Options{
//	    CacheDir:   "/var/lib/loom-cloud/certs",
//	    Email:      "ops@loom.dev",
//	    BaseDomain: "loom.dev",
//	    AllowSlug:  func(slug string) bool { return siteRepo.Exists(slug) },
//	})
//	srv := &http.Server{
//	    Addr:      ":443",
//	    TLSConfig: tlsMgr.TLSConfig(),
//	    Handler:   handler,
//	}
//	go http.ListenAndServe(":80", tlsMgr.HTTPHandler(nil)) // ACME http-01
//	srv.ListenAndServeTLS("", "")
package tls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/acme/autocert"
)

// Options configures Manager. Email is the contact address Let's
// Encrypt uses for renewal warnings; BaseDomain bounds which
// hostnames the policy will issue for; AllowSlug is the
// per-request "is this a real tenant?" check.
type Options struct {
	// CacheDir is where autocert persists private keys + issued
	// certs. Must be writable by the running process; persistent
	// across restarts so we don't re-issue on every reboot (and
	// don't burn the LE rate limit).
	CacheDir string
	// Email goes into the ACME account registration; LE sends
	// renewal-failure emails here. Empty is technically allowed
	// by autocert but you'll never know your certs are dying —
	// strongly recommended to set it.
	Email string
	// BaseDomain bounds the HostPolicy. Only "<slug>.<base>"
	// hostnames pass; the apex itself doesn't (production
	// typically serves marketing on a different host).
	BaseDomain string
	// AllowSlug checks whether the supplied tenant slug exists
	// in the platform's Site table. Returning false rejects the
	// HostPolicy check and autocert refuses the handshake — so
	// a host that pointed *.loom.dev at our IP without being a
	// real tenant gets a TLS handshake error, not a free cert.
	//
	// nil is treated as "every slug is valid" — fine for local
	// staging but never appropriate in production.
	AllowSlug func(slug string) bool
	// Staging, when true, points autocert at LE's staging
	// directory (no rate limits; certs aren't browser-trusted).
	// Set this in CI / smoke tests; never in production.
	Staging bool
}

// Manager wraps autocert.Manager with the loom-cloud-specific
// HostPolicy and provides the http.Server wiring helpers.
type Manager struct {
	auto *autocert.Manager
	opts Options
}

// NewManager constructs a Manager with the supplied options.
// Returns an error when CacheDir or BaseDomain is empty.
func NewManager(opts Options) (*Manager, error) {
	if opts.CacheDir == "" {
		return nil, errors.New("tls: Options.CacheDir is required")
	}
	if opts.BaseDomain == "" {
		return nil, errors.New("tls: Options.BaseDomain is required")
	}
	m := &Manager{opts: opts}

	auto := &autocert.Manager{
		Cache:      autocert.DirCache(opts.CacheDir),
		Prompt:     autocert.AcceptTOS,
		Email:      opts.Email,
		HostPolicy: m.hostPolicy,
	}
	if opts.Staging {
		auto.Client = stagingClient()
	}
	m.auto = auto
	return m, nil
}

// TLSConfig returns the *tls.Config to install on an
// http.Server. autocert handles SNI-based cert lookup +
// on-demand issuance.
func (m *Manager) TLSConfig() *tls.Config {
	cfg := m.auto.TLSConfig()
	// Pin minimum to TLS 1.2 — autocert defaults are reasonable
	// but we want belt-and-braces on cipher floor for a hosted
	// product.
	cfg.MinVersion = tls.VersionTLS12
	return cfg
}

// HTTPHandler wraps `fallback` so port-80 traffic serves the
// ACME http-01 challenges autocert needs. fallback may be nil,
// in which case non-ACME traffic gets redirected to https://.
func (m *Manager) HTTPHandler(fallback http.Handler) http.Handler {
	return m.auto.HTTPHandler(fallback)
}

// hostPolicy is the autocert.HostPolicy that gates which hosts
// we'll issue certs for.
//
// Rules:
//
//	1. Host must be exactly "<slug>.<BaseDomain>" — the apex
//	   itself is rejected (set up an apex policy explicitly if
//	   you need it; it's a different concern).
//	2. Slug must pass AllowSlug — the per-tenant existence
//	   check. nil AllowSlug accepts all slugs (dev only).
func (m *Manager) hostPolicy(_ context.Context, host string) error {
	suffix := "." + m.opts.BaseDomain
	if !strings.HasSuffix(host, suffix) {
		return fmt.Errorf("tls: host %q is not a *.%s subdomain", host, m.opts.BaseDomain)
	}
	slug := strings.TrimSuffix(host, suffix)
	if slug == "" || strings.Contains(slug, ".") {
		// Reject apex AND multi-label hosts (foo.bar.loom.dev) —
		// loom-cloud serves single-label tenants only.
		return fmt.Errorf("tls: host %q is not a single-label tenant", host)
	}
	if m.opts.AllowSlug == nil {
		return nil
	}
	if !m.opts.AllowSlug(slug) {
		return fmt.Errorf("tls: slug %q is not a known tenant", slug)
	}
	return nil
}
