package main

// ci_shared.go — what did NOT go to internal/cigate.
//
// tfvars.ReadRegion knows the repo's terraform directory layout; linode.ClientFromEnv is the
// CI PAT reader. Neither is a gate primitive, and a shared package that knew
// either would be a shared package that knows this repo's shape.
