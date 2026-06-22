package clusterspec

// instance.go is the layout-aware front door. An instance authors its spec in
// one of two on-disk shapes (docs/landing-zone-spec.md):
//
//   - single-file (simple mode): one llz.yaml (kind: LandingZone) with every
//     environment inline under spec.environments;
//   - split (the fleet default): a landingzone.yaml (instance identity + shared
//     spec.defaults) plus one clusters/<env>.yaml (kind: ClusterDefinition,
//     metadata.name == env) per deployment, each inheriting the defaults.
//
// Both assemble into the SAME in-memory *LandingZone (spec.environments keyed by
// deployment), so the renderer, the tfvars mapping, and every validator are
// shape-agnostic — the split layout is the CRD-faithful representation (one
// LandingZone object + one ClusterDefinition per env) without a second model.

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
	// ClustersDir holds one ClusterDefinition per environment.
	ClustersDir = "clusters"
	// KindClusterDefinition is the per-env resource kind in the split layout.
	KindClusterDefinition = "ClusterDefinition"
)

// ErrNoSpec is returned by LoadInstance when neither layout is present at the
// instance root — callers treat it as "this instance has not adopted the spec"
// (the no-op contract) rather than a hard failure.
var ErrNoSpec = errors.New("no LandingZone spec (llz.yaml or landingzone.yaml) found")

// ClusterDefinition is one environment in the split layout. Its spec is exactly
// an Environment (cluster + recipes), so a clusters/<env>.yaml ≅ the inline
// spec.environments.<env> of the single-file layout — the same fields, just
// relocated to their own file.
type ClusterDefinition struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata"`
	Spec       Environment `json:"spec"`
}

// InstancePresent reports whether either layout exists at root.
func InstancePresent(root string) bool {
	return Exists(filepath.Join(root, DefaultFile)) ||
		Exists(filepath.Join(root, LandingZoneFile))
}

// LoadInstance loads the assembled, defaulted LandingZone for the instance at
// root, auto-detecting the layout. Returns ErrNoSpec when neither file exists,
// and an error if both are present (an ambiguous mix the operator must resolve).
func LoadInstance(root string) (*LandingZone, error) {
	single := filepath.Join(root, DefaultFile)
	split := filepath.Join(root, LandingZoneFile)
	switch {
	case Exists(single) && Exists(split):
		return nil, fmt.Errorf("both %s and %s present at %q — keep one layout (see docs/landing-zone-spec.md)", DefaultFile, LandingZoneFile, root)
	case Exists(single):
		return Load(single)
	case Exists(split):
		return LoadSplit(root)
	default:
		return nil, ErrNoSpec
	}
}

// LoadSplit assembles the split layout at root: landingzone.yaml (identity +
// defaults) plus every clusters/*.yaml (one ClusterDefinition each). Each env
// inherits spec.defaults, then the built-in Defaults() fill the rest. Structural
// problems (bad kind, missing/duplicate name) error here; semantic ones are left
// for Validate, mirroring how Load defers to Validate for the single-file shape.
func LoadSplit(root string) (*LandingZone, error) {
	lz, err := Load(filepath.Join(root, LandingZoneFile))
	if err != nil {
		return nil, err
	}
	// Load already applied inheritance + Defaults to any inline environments;
	// re-running below after adding the cluster files is idempotent.
	if lz.Spec.Environments == nil {
		lz.Spec.Environments = map[string]Environment{}
	}

	files, err := filepath.Glob(filepath.Join(root, ClustersDir, "*.yaml"))
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

// loadClusterDefinition reads + structurally validates one clusters/<env>.yaml.
func loadClusterDefinition(path string) (*ClusterDefinition, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cd ClusterDefinition
	if err := yaml.UnmarshalStrict(b, &cd); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cd.Kind != KindClusterDefinition {
		return nil, fmt.Errorf("%s: kind %q invalid (want %q)", path, cd.Kind, KindClusterDefinition)
	}
	if cd.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: apiVersion %q invalid (want %q)", path, cd.APIVersion, APIVersion)
	}
	if cd.Metadata.Name == "" {
		return nil, fmt.Errorf("%s: metadata.name is required (it is the deployment/env name)", path)
	}
	return &cd, nil
}
