package dns

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordContent(t *testing.T, p *MemoryProvider, name string) string {
	t.Helper()
	recs, err := p.List(context.Background())
	require.NoError(t, err)
	for _, r := range recs {
		if r.Name == name && r.Type == "A" {
			return r.Content
		}
	}
	return ""
}

func TestRegionTable_Select(t *testing.T) {
	table := NewRegionTable([]Region{
		{Name: "us-east", EdgeIP: "203.0.113.1"},
		{Name: "eu-west", EdgeIP: "203.0.113.2"},
		{Name: "bad", EdgeIP: ""}, // dropped (no IP)
	})
	ip, ok := table.Select("us-east")
	assert.True(t, ok)
	assert.Equal(t, "203.0.113.1", ip)

	_, ok = table.Select("ap-south")
	assert.False(t, ok)
	_, ok = table.Select("bad")
	assert.False(t, ok, "region with empty IP not registered")
}

func TestEnsureSlugInRegion_PublishesRegionalIP(t *testing.T) {
	p := NewMemoryProvider()
	m := &Manager{
		Provider:   p,
		BaseDomain: "loom.dev",
		EdgeIP:     "203.0.113.99", // global fallback, should NOT be used
		Regions: NewRegionTable([]Region{
			{Name: "us-east", EdgeIP: "203.0.113.1"},
			{Name: "eu-west", EdgeIP: "203.0.113.2"},
		}),
	}

	require.NoError(t, m.EnsureSlugInRegion(context.Background(), "acme", "eu-west"))
	assert.Equal(t, "203.0.113.2", recordContent(t, p, "acme.loom.dev"))
}

func TestEnsureSlugInRegion_EmptyRegionFallsBackToEdgeIP(t *testing.T) {
	p := NewMemoryProvider()
	m := &Manager{
		Provider:   p,
		BaseDomain: "loom.dev",
		EdgeIP:     "203.0.113.99",
		Regions:    NewRegionTable([]Region{{Name: "us-east", EdgeIP: "203.0.113.1"}}),
	}
	require.NoError(t, m.EnsureSlugInRegion(context.Background(), "single", ""))
	assert.Equal(t, "203.0.113.99", recordContent(t, p, "single.loom.dev"))
}

func TestEnsureSlugInRegion_UnknownRegionErrors(t *testing.T) {
	p := NewMemoryProvider()
	m := &Manager{
		Provider:   p,
		BaseDomain: "loom.dev",
		EdgeIP:     "203.0.113.99",
		Regions:    NewRegionTable([]Region{{Name: "us-east", EdgeIP: "203.0.113.1"}}),
	}
	err := m.EnsureSlugInRegion(context.Background(), "acme", "ap-south")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ap-south")
	// Nothing published for the misplaced site.
	assert.Equal(t, "", recordContent(t, p, "acme.loom.dev"))
}

func TestEnsureSlugInRegion_NoRegionsUsesEdgeIP(t *testing.T) {
	p := NewMemoryProvider()
	m := &Manager{Provider: p, BaseDomain: "loom.dev", EdgeIP: "203.0.113.10"}
	require.NoError(t, m.EnsureSlugInRegion(context.Background(), "acme", "us-east"))
	// Regions nil → region ignored, EdgeIP used.
	assert.Equal(t, "203.0.113.10", recordContent(t, p, "acme.loom.dev"))
}

func TestEnsureSlugInRegion_Validations(t *testing.T) {
	ctx := context.Background()
	// No provider.
	err := (&Manager{BaseDomain: "loom.dev"}).EnsureSlugInRegion(ctx, "x", "")
	require.Error(t, err)
	// No base domain.
	err = (&Manager{Provider: NewMemoryProvider()}).EnsureSlugInRegion(ctx, "x", "")
	require.Error(t, err)
	// No slug.
	err = (&Manager{Provider: NewMemoryProvider(), BaseDomain: "loom.dev", EdgeIP: "1.2.3.4"}).EnsureSlugInRegion(ctx, "", "")
	require.Error(t, err)
	// Empty region + no EdgeIP → no target.
	err = (&Manager{Provider: NewMemoryProvider(), BaseDomain: "loom.dev"}).EnsureSlugInRegion(ctx, "x", "")
	require.Error(t, err)
}
