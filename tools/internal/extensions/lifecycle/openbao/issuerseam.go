package openbao

// issuerseam.go — the cluster-issuer lookup, as a seam rather than an import.
//
// teamlogin.go called identityconfig.DiscoverIssuerFromCluster directly, and that
// import is a CYCLE — not in the build, but in the TEST build:
//
//	identityconfig (test) -> reconcilelanes -> openbao -> identityconfig
//
// Go reports it as "import cycle not allowed in test", which is easy to misread as
// a test problem. It is not: it is the build telling you that the identity plane
// and the OpenBao client both want to own "which Keycloak issues our tokens", and
// only one of them can.
//
// The seam settles it in OpenBao's favour for this call only: the CLIENT asks a
// question, package main installs the ANSWER. The default returns "" rather than
// guessing, because the caller's next line falls back to the spec-derived issuer
// and an empty string is what makes that fallback run.
var DiscoverIssuerFromCluster = func() string { return "" }
