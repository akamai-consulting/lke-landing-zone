package clusterspec

// instance.go is the spec front door (docs/landing-zone-spec.md). An instance is
// authored as the split layout: a landingzone.yaml (instance identity + shared
// spec.defaults) plus one environments/<env>.yaml (kind: ClusterDefinition,
// metadata.name == env) per deployment, each inheriting the defaults.
//
// The loader assembles these into an in-memory *LandingZone (spec.environments
// keyed by deployment) — the CRD-faithful shape (one LandingZone object + one
// ClusterDefinition per env) the renderer, tfvars mapping, and validators all
// consume. spec.environments is the ASSEMBLED model only; authoring it inline in
// landingzone.yaml is rejected (LoadSplit) so environments/ stays the single source.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"
)

const (
	// LandingZoneFile is the split layout's root (identity + shared defaults).
	LandingZoneFile = "landingzone.yaml"
	// EnvironmentsDir holds one ClusterDefinition per environment.
	EnvironmentsDir = "environments"
	// KindClusterDefinition is the per-env resource kind in the split layout.
	KindClusterDefinition = "ClusterDefinition"
)

// ErrNoSpec is returned by LoadInstance when no landingzone.yaml is present at
// the instance root — callers treat it as "this instance has not adopted the
// spec" (the no-op contract) rather than a hard failure.
var ErrNoSpec = errors.New("no LandingZone spec (landingzone.yaml) found")

// ClusterDefinition is one environment in the split layout. Its spec is exactly
// an Environment (cluster + recipes): environments/<env>.yaml carries one cluster
// definition + its recipe toggles, inheriting landingzone.yaml's spec.defaults.
type ClusterDefinition struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata"`
	Spec       Environment `json:"spec"`
}

// InstancePresent reports whether a spec (landingzone.yaml) exists at root.
func InstancePresent(root string) bool {
	return Exists(filepath.Join(root, LandingZoneFile))
}

// LoadInstance loads the assembled, defaulted LandingZone for the instance at
// root. Returns ErrNoSpec when no landingzone.yaml exists.
func LoadInstance(root string) (*LandingZone, error) {
	if !InstancePresent(root) {
		return nil, ErrNoSpec
	}
	return LoadSplit(root)
}

// LoadSplit assembles the spec at root: landingzone.yaml (identity + defaults)
// plus every environments/*.yaml (one ClusterDefinition each). Each env inherits
// spec.defaults, then the built-in Defaults() fill the rest. Structural problems
// (bad kind, missing/duplicate name, environments authored inline in
// landingzone.yaml) error here; semantic ones are left for Validate.
func LoadSplit(root string) (*LandingZone, error) {
	lz, err := Load(filepath.Join(root, LandingZoneFile))
	if err != nil {
		return nil, err
	}
	// Deployments are authored as environments/<env>.yaml, never inline — keep
	// landingzone.yaml to identity + shared defaults so environments/ is the one place
	// an env is defined.
	if len(lz.Spec.Environments) > 0 {
		return nil, fmt.Errorf("%s must not define spec.environments — author each deployment as %s/<env>.yaml", LandingZoneFile, EnvironmentsDir)
	}
	lz.Spec.Environments = map[string]Environment{}

	files, err := filepath.Glob(filepath.Join(root, EnvironmentsDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		cd, err := loadClusterDefinition(f)
		if err != nil {
			return nil, err
		}
		name := cd.Metadata.Name
		if _, dup := lz.Spec.Environments[name]; dup {
			return nil, fmt.Errorf("%s: duplicate environment %q (already defined)", f, name)
		}
		lz.Spec.Environments[name] = cd.Spec
	}

	lz.applyInheritance()
	lz.Defaults()
	return lz, nil
}

// loadClusterDefinition reads + structurally validates one environments/<env>.yaml.
func loadClusterDefinition(path string) (*ClusterDefinition, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cd, err := DecodeClusterDefinition(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cd, nil
}

// DecodeClusterDefinition strict-parses one environments/<env>.yaml's bytes,
// rejecting unknown fields (a typo'd path). Exposed so a CLI write command can
// validate a single edited file before committing it.
func DecodeClusterDefinition(b []byte) (*ClusterDefinition, error) {
	var cd ClusterDefinition
	if err := yaml.UnmarshalStrict(b, &cd); err != nil {
		return nil, err
	}
	if cd.Kind != KindClusterDefinition {
		return nil, fmt.Errorf("kind %q invalid (want %q)", cd.Kind, KindClusterDefinition)
	}
	if cd.APIVersion != APIVersion {
		return nil, fmt.Errorf("apiVersion %q invalid (want %q)", cd.APIVersion, APIVersion)
	}
	if cd.Metadata.Name == "" {
		return nil, fmt.Errorf("metadata.name is required (the deployment/env name)")
	}
	return &cd, nil
}
