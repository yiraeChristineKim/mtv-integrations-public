package main

import (
	"crypto/tls"

	apiconfigv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
)

// tlsConfigFromProfileSpec builds a tls.Config applier from the hub APIServer TLS profile spec.
// When profile.Groups is empty, CurvePreferences are not set (Go default group selection).
func tlsConfigFromProfileSpec(profileSpec apiconfigv1.TLSProfileSpec) (func(*tls.Config), []string) {
	return tlspkg.NewTLSConfigFromProfile(profileSpec)
}
