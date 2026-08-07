package versionpins

// citags.go — the CI toolchain tags an instance is scaffolded against.
//
// THEY BELONG WITH THE GATE THAT CHECKS THEM. This package already reads
// dockerfiles/Dockerfile as the version authority and fails when a restatement
// drifts from it; these two consts ARE restatements, and they lived in a 700-line
// file about GitHub tokens with three files reaching in for them.
//
// The comment below is the original and it is a scar: ciTofuTag was still on 1.9.8
// after build-images.yml and lint.yml had both moved, which would have scaffolded
// new instances onto a HashiCorp Terraform image while every caller invoked
// `tofu`. That is the drift this package exists to catch, so the value it catches
// drift in should not live somewhere else.

// CI image tags published by build-images.yml; TF_IMAGE/KUBE_IMAGE derive from
// these + the template org.
// A THIRD restatement of the image pin, beyond the two the Dockerfile header
// names (build-images.yml's matrix and lint.yml's fallback). It was still on
// 1.9.8 after both of those moved, which would have scaffolded new instances
// onto a HashiCorp Terraform image while every caller invoked `tofu`.
const (
	CITofuTag       = "1.12.5"
	CIKubernetesTag = "1.31.0"
)
