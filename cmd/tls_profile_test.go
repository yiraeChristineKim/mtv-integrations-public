package main

import (
	"crypto/tls"
	"testing"

	apiconfigv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLSConfigFromProfileSpec_groupsUnset(t *testing.T) {
	t.Parallel()

	spec := apiconfigv1.TLSProfileSpec{
		MinTLSVersion: apiconfigv1.VersionTLS12,
		Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
		Groups:        nil,
	}

	apply, unsupported := tlsConfigFromProfileSpec(spec)
	require.Empty(t, unsupported)

	cfg := &tls.Config{}
	apply(cfg)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.Nil(t, cfg.CurvePreferences)
}

func TestTLSConfigFromProfileSpec_groupsFilter(t *testing.T) {
	t.Parallel()

	spec := apiconfigv1.TLSProfileSpec{
		MinTLSVersion: apiconfigv1.VersionTLS13,
		Ciphers:       []string{"TLS_AES_128_GCM_SHA256"},
		Groups: []apiconfigv1.TLSGroup{
			apiconfigv1.TLSGroupX25519,
			apiconfigv1.TLSGroupSecP256r1,
		},
	}

	apply, unsupported := tlsConfigFromProfileSpec(spec)
	require.Empty(t, unsupported)

	cfg := &tls.Config{}
	apply(cfg)
	require.NotEmpty(t, cfg.CurvePreferences)
	assert.Contains(t, cfg.CurvePreferences, tls.X25519)
	assert.Contains(t, cfg.CurvePreferences, tls.CurveP256)
}

func TestTLSConfigFromProfileSpec_intermediateProfileIncludesGroups(t *testing.T) {
	t.Parallel()

	profile := *apiconfigv1.TLSProfiles[apiconfigv1.TLSProfileIntermediateType]
	require.NotEmpty(t, profile.Groups, "named profiles should carry default groups from openshift/api")

	apply, _ := tlsConfigFromProfileSpec(profile)
	cfg := &tls.Config{}
	apply(cfg)
	assert.NotEmpty(t, cfg.CurvePreferences)
}
