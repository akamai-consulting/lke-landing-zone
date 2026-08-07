package openbao

// DefaultAddr is where OpenBao answers in-cluster.
//
// It moved here from internal/reconcilelanes when the in-cluster kubernetes-auth
// login moved into this package and produced an import cycle:
// openbao -> reconcilelanes ->  The cycle was the question being asked
// out loud — a lane package was holding an OpenBao FACT. reconcilelanes keeps its
// DefaultOpenBaoAddr name as an alias so its own callers do not churn, but there
// is one literal now, and it is in the package named for the thing it addresses.
//
// Port 8200 is the SERVICE listener, not the pod's 8210 loopback (baoread.LoopbackAddr).
// A caller inside the pod must use the second; anything else in the cluster uses this.
const DefaultAddr = "https://platform-llz-svc.cluster.local:8200"
