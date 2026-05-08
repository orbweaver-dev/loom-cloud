package tls

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager_RequiresCacheDir(t *testing.T) {
	_, err := NewManager(Options{BaseDomain: "loom.dev"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CacheDir")
}

func TestNewManager_RequiresBaseDomain(t *testing.T) {
	_, err := NewManager(Options{CacheDir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BaseDomain")
}

func TestNewManager_BasicConstruction(t *testing.T) {
	m, err := NewManager(Options{
		CacheDir:   filepath.Join(t.TempDir(), "certs"),
		BaseDomain: "loom.dev",
		Email:      "ops@example.com",
	})
	require.NoError(t, err)
	require.NotNil(t, m)
	assert.NotNil(t, m.TLSConfig())
}

func TestHostPolicy_RejectsForeignDomain(t *testing.T) {
	m, _ := NewManager(Options{CacheDir: t.TempDir(), BaseDomain: "loom.dev"})
	err := m.hostPolicy(context.Background(), "evil.example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a *.loom.dev")
}

func TestHostPolicy_RejectsApex(t *testing.T) {
	m, _ := NewManager(Options{CacheDir: t.TempDir(), BaseDomain: "loom.dev"})
	err := m.hostPolicy(context.Background(), "loom.dev")
	require.Error(t, err)
}

func TestHostPolicy_RejectsMultiLabel(t *testing.T) {
	m, _ := NewManager(Options{CacheDir: t.TempDir(), BaseDomain: "loom.dev"})
	err := m.hostPolicy(context.Background(), "a.b.loom.dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single-label")
}

func TestHostPolicy_AllowsKnownSlug(t *testing.T) {
	m, _ := NewManager(Options{
		CacheDir:   t.TempDir(),
		BaseDomain: "loom.dev",
		AllowSlug:  func(s string) bool { return s == "acme" },
	})
	require.NoError(t, m.hostPolicy(context.Background(), "acme.loom.dev"))
}

func TestHostPolicy_RejectsUnknownSlug(t *testing.T) {
	m, _ := NewManager(Options{
		CacheDir:   t.TempDir(),
		BaseDomain: "loom.dev",
		AllowSlug:  func(s string) bool { return s == "acme" },
	})
	err := m.hostPolicy(context.Background(), "stranger.loom.dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known tenant")
}

func TestHostPolicy_NilAllowSlug_AllowsAny(t *testing.T) {
	m, _ := NewManager(Options{CacheDir: t.TempDir(), BaseDomain: "loom.dev"})
	require.NoError(t, m.hostPolicy(context.Background(), "anything.loom.dev"))
}

func TestStaging_UsesStagingDirectory(t *testing.T) {
	m, err := NewManager(Options{
		CacheDir:   t.TempDir(),
		BaseDomain: "loom.dev",
		Staging:    true,
	})
	require.NoError(t, err)
	assert.Contains(t, m.auto.Client.DirectoryURL, "staging")
}

func TestTLSConfig_PinsTLS12Minimum(t *testing.T) {
	m, _ := NewManager(Options{CacheDir: t.TempDir(), BaseDomain: "loom.dev"})
	cfg := m.TLSConfig()
	// uint16 0x0303 = TLS 1.2
	assert.Equal(t, uint16(0x0303), cfg.MinVersion)
}
