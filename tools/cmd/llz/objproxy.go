package main

// objproxy.go — `llz obj-proxy`, the in-cluster S3 gateway that makes Linode
// Object Storage encrypted at rest for writers that cannot ask for it themselves.
//
// See objproxy_inject.go for WHY this shape (SSE-C header injection, no
// re-signing) and for the measurements behind it. This file is the process: TLS
// in, streamed reverse proxy out, plus the two operational properties that decide
// whether it is safe to put on the path of every image pull and log write.
//
// PROPERTY 1 — IT MUST NOT PROXY TO ITSELF. Callers reach this process because
// CoreDNS rewrites <region>.linodeobjects.com to its Service, and the upstream
// this dials is THAT SAME NAME. Resolving it through cluster DNS would return the
// proxy's own address and every request would loop until it exhausted something.
// The deployment sets `dnsPolicy: Default`, so this pod resolves via the NODE's
// resolver, which carries no rewrite. That is a load-bearing manifest setting, not
// a detail, so the proxy verifies at startup that the upstream does not resolve to
// one of its own addresses and refuses to serve if it does — a loop that is caught
// at boot is an outage; one that is not is an outage plus a mystery.
//
// PROPERTY 2 — IT STREAMS. Harbor pushes multi-gigabyte layers. httputil's
// ReverseProxy streams request and response bodies without buffering, and nothing
// here reads or rewrites a body, so memory is flat in object size. Any future
// change that touches a body must revisit that.
//
// WHAT IT DOES NOT DO. It does not re-sign (measured unnecessary), does not
// terminate the client's SigV4 relationship with Linode, does not cache, and does
// not encrypt anything itself. It also cannot help objects written BEFORE it was
// introduced: those are plaintext, and a GET carrying SSE-C headers against a
// plaintext object fails. Migration is a separate, deliberate step.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// objProxyLoopHeader marks a request as having already passed through this proxy.
//
// IT IS THE REAL LOOP PROTECTION. assertNotSelf below can only see the case where
// the upstream resolves to the POD's own address — but the CoreDNS rewrite points
// the endpoint hostname at the obj-proxy SERVICE, whose ClusterIP is not a pod
// interface address. So the shape the startup check was written for (cluster DNS
// reaching the proxy instead of the node resolver) resolves to the Service, passes
// the check, and loops through kube-proxy back into a proxy pod — possibly a
// different one each hop, so no single process ever sees itself.
//
// A marker header catches every shape of that regardless of DNS, in one comparison,
// and it does so on the FIRST looped request rather than after something exhausts.
// Not an x-amz-* name on purpose: S3 ignores unknown headers, and this must never
// be mistaken for a request parameter.
const objProxyLoopHeader = "X-Llz-Obj-Proxy"

type objProxyOpts struct {
	listen      string
	upstream    string
	keyFile     string
	tlsCert     string
	tlsKey      string
	healthAddr  string
	upstreamCAs string
	credsFile   string
	creds       objProxyCreds
	// credsFn, when set, is consulted per request so a rotated credential is picked
	// up without a restart. Tests set creds directly instead.
	credsFn func() objProxyCreds
}

// objProxyCounters are the numbers an operator needs to answer "is it actually
// encrypting?" without trusting the configuration. Exposed on the health port.
type objProxyCounters struct {
	total     atomic.Uint64
	injected  atomic.Uint64
	passed    atomic.Uint64
	copySrc   atomic.Uint64
	upstreamE atomic.Uint64
	loops     atomic.Uint64
	resigned  atomic.Uint64
	resignErr atomic.Uint64
	// misdirected counts virtual-host-style requests refused rather than written
	// unencrypted. Non-zero means something is addressing object storage in a form
	// this gateway cannot protect.
	misdirected atomic.Uint64
}

func objProxyCmd() *cobra.Command {
	var o objProxyOpts
	c := &cobra.Command{
		Use:   "obj-proxy",
		Short: "S3 gateway that adds SSE-C encryption to Linode Object Storage writes",
		Long: "Reverse-proxies S3 traffic to Linode Object Storage, injecting SSE-C\n" +
			"encryption headers that Loki and Harbor cannot emit themselves. Linode\n" +
			"implements no other server-side encryption mode (SSE-S3 is rejected 400;\n" +
			"PutBucketEncryption is 501), so this is what makes those buckets encrypted\n" +
			"at rest — with a key Linode never stores.\n\n" +
			"Bodies are streamed and never inspected; the proxy performs no cryptography\n" +
			"of its own and does not re-sign requests (SSE-C headers are honoured outside\n" +
			"SigV4 SignedHeaders, which is why callers must reach this at the real\n" +
			"endpoint hostname via DNS rather than at a Service name).\n\n" +
			"KEY LOSS IS DATA LOSS: Linode discards SSE-C keys on receipt, so an object\n" +
			"written under a lost key is unrecoverable by anyone. Escrow --key-file with\n" +
			"the same discipline as the OpenBao seal key.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runObjProxy(cmd.Context(), o) },
	}
	f := c.Flags()
	f.StringVar(&o.listen, "listen", ":8443", "HTTPS listen address for S3 clients")
	f.StringVar(&o.upstream, "upstream", "", "real Object Storage endpoint host, e.g. us-ord-10.linodeobjects.com (required)")
	f.StringVar(&o.credsFile, "creds-file", "",
		"optional file holding the object-storage AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY this proxy may "+
			"re-sign with. Enables the #397 repair: requests sent in the aws-chunked trailer-checksum framing "+
			"Linode rejects are de-chunked and re-signed AS THE SAME ACCESS KEY. Without it the proxy never "+
			"re-signs and such requests fail upstream exactly as they do today")
	f.StringVar(&o.keyFile, "key-file", "", "file holding the 32-byte raw SSE-C key (required)")
	f.StringVar(&o.tlsCert, "tls-cert", "", "serving certificate for the endpoint hostname (required)")
	f.StringVar(&o.tlsKey, "tls-key", "", "serving private key (required)")
	f.StringVar(&o.healthAddr, "health-addr", ":8080", "plaintext /healthz + /stats address for kubelet probes")
	f.StringVar(&o.upstreamCAs, "upstream-ca-file", "", "optional CA bundle for verifying the upstream (default: system roots)")
	return c
}

func runObjProxy(ctx context.Context, o objProxyOpts) error {
	for _, req := range []struct{ name, val string }{
		{"--upstream", o.upstream}, {"--key-file", o.keyFile},
		{"--tls-cert", o.tlsCert}, {"--tls-key", o.tlsKey},
	} {
		if req.val == "" {
			return fmt.Errorf("%s is required", req.name)
		}
	}

	o.upstream = upstreamHostOnly(o.upstream)

	raw, err := os.ReadFile(o.keyFile)
	if err != nil {
		return fmt.Errorf("read SSE-C key: %w", err)
	}
	material, err := parseSSECKeyFile(raw)
	if err != nil {
		return err
	}
	key, err := newSSECKey(material)
	if err != nil {
		return err
	}

	// Optional, and its absence is a normal configuration rather than a fault: with
	// no credentials the proxy never re-signs and behaves exactly as it did before
	// #397. An UNREADABLE file is a fault, though — it means someone intended the
	// repair and it silently would not happen.
	if o.credsFile != "" {
		rawCreds, err := os.ReadFile(o.credsFile)
		if err != nil {
			return fmt.Errorf("read object-storage credentials: %w", err)
		}
		o.creds = parseObjProxyCreds(rawCreds)
		loader := &credsLoader{path: o.credsFile}
		o.credsFn = loader.get
		if !o.creds.usable() {
			return fmt.Errorf("%s holds no usable AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY pair — "+
				"the #397 re-signing repair would be silently off", o.credsFile)
		}
		fmt.Printf("obj-proxy: re-signing enabled for access key %s (repairs the aws-chunked trailer framing Linode rejects)\n",
			o.creds.AccessKeyID)
	}

	if err := assertNotSelf(o.upstream); err != nil {
		return err
	}

	counters := &objProxyCounters{}
	proxy, err := buildObjProxy(o, key, counters)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              o.listen,
		Handler:           objProxyHandlerForHost(proxy, counters, o.upstream),
		ReadHeaderTimeout: 15 * time.Second,
		TLSConfig:         objProxyServingTLS(o.tlsCert, o.tlsKey),
	}
	health := &http.Server{Addr: o.healthAddr, Handler: objProxyHealth(counters), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("health server: %w", err)
		}
	}()
	go func() {
		// Empty strings: the keypair comes from TLSConfig.GetCertificate, not from
		// these arguments. Passing the paths here would ALSO load them once at
		// startup and silently win for connections that do not hit GetCertificate.
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	fmt.Printf("obj-proxy: listening on %s, upstream https://%s, SSE-C key %d bytes (md5 %s)\n",
		o.listen, o.upstream, sseCustomerKeyRawBytes, key.md5B64)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = health.Shutdown(shutCtx)
		return srv.Shutdown(shutCtx)
	}
}

// parseSSECKeyFile turns the mounted key file into 32 raw AES-256 bytes.
//
// IT MUST ACCEPT BASE64, and that is the whole point. `llz ci seed-ssec-key`
// writes the key to OpenBao base64-encoded, because a KV value is a JSON STRING
// and raw AES bytes contain NULs and invalid UTF-8 — there is no wire format here
// that carries them intact. ESO then materialises that string verbatim into the
// Secret, so the file on disk is 44 base64 characters, not 32 bytes.
//
// The first revision required exactly 32 raw bytes and CrashLoopBackOff'd every
// pod in the first e2e run that got as far as starting the process. Both sides
// were individually tested and individually correct: the seeder's test asserted it
// wrote base64 of 32 bytes, the reader's asserted it required 32 raw bytes. Neither
// test ever saw the other's output, which is exactly why
// TestSSECKeyRoundTripsFromSeederToProxy now exists.
//
// A RAW 32-byte file is still accepted, for an operator who does
// `openssl rand 32 > key` by hand.
//
// THE TWO FORMS CAN BE CONFUSED, contrary to what an earlier version of this
// comment claimed — a test caught it. A 32-character ASCII key drawn from the
// base64 alphabet decodes cleanly as base64, to 24 bytes. So decoding succeeding is
// NOT evidence the input was base64; only decoding to exactly 32 bytes is. A
// decode that yields any other length therefore falls THROUGH to the raw check
// rather than failing, and only an input that is neither is an error.
func parseSSECKeyFile(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimRight(raw, "\r\n")

	if decoded, err := base64.StdEncoding.DecodeString(string(trimmed)); err == nil {
		if len(decoded) == sseCustomerKeyRawBytes {
			return decoded, nil
		}
		// Decoded to the wrong length: this was probably never base64. Fall through.
	}
	if len(trimmed) == sseCustomerKeyRawBytes {
		return trimmed, nil // a raw key written by hand
	}
	return nil, fmt.Errorf(
		"the SSE-C key file is neither base64 of %d bytes nor %d raw bytes (got %d bytes on disk). "+
			"The in-cluster path is base64: `llz ci seed-ssec-key` writes it that way because a KV "+
			"value is a JSON string and raw AES bytes do not survive one",
		sseCustomerKeyRawBytes, sseCustomerKeyRawBytes, len(trimmed))
}

// buildObjProxy assembles the reverse proxy. Split out so tests drive the whole
// handler against an httptest upstream instead of only the injection function.
// resolveCreds prefers the live loader (so a rotation is seen without a restart)
// and falls back to the static value tests supply.
func resolveCreds(o objProxyOpts) objProxyCreds {
	if o.credsFn != nil {
		return o.credsFn()
	}
	return o.creds
}

func buildObjProxy(o objProxyOpts, key ssecKey, c *objProxyCounters) (*httputil.ReverseProxy, error) {
	target, err := url.Parse("https://" + o.upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	transport, err := objProxyTransport(o.upstreamCAs)
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(r *http.Request) {
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			// Host is deliberately UNCHANGED from what the client signed. The client
			// addressed the real endpoint name (DNS sent it here), so preserving it is
			// what keeps its SigV4 signature valid without re-signing.
			r.Host = o.upstream
			c.total.Add(1)
			if r.Header.Get(hdrCopySource) != "" {
				c.copySrc.Add(1)
			}
			// Mark it so a request that comes back round is recognisable. Unsigned,
			// like the SSE-C headers, which is measured-safe: the upstream honours
			// headers outside SignedHeaders and ignores ones it does not know.
			r.Header.Set(objProxyLoopHeader, "1")
			// #397: repair the framing Linode rejects, BEFORE the SSE-C headers go on.
			// Order matters — re-signing rewrites Authorization over a minimal signed
			// set, and the SSE-C headers must stay outside it exactly as they do on a
			// pass-through request, so that the two paths differ in as little as
			// possible. Failure here is non-fatal: the original request is forwarded
			// untouched and the upstream's own 403 is a truer verdict than a rewrite
			// we could not complete.
			if ok, err := resignForUpstream(r, resolveCreds(o), o.upstream); err != nil {
				c.resignErr.Add(1)
				fmt.Fprintf(os.Stderr, "obj-proxy: could not re-sign %s %s (%v) — forwarding as-is\n", r.Method, r.URL.Path, err)
			} else if ok {
				c.resigned.Add(1)
			}
			if injectSSEC(r.Header, r.Method, r.URL.Path, key) {
				c.injected.Add(1)
			} else {
				c.passed.Add(1)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			c.upstreamE.Add(1)
			// Never echo the request headers: they carry the SSE-C key.
			fmt.Fprintf(os.Stderr, "obj-proxy: upstream error for %s %s: %v\n", r.Method, r.URL.Path, err)
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
	return rp, nil
}

// objProxyHandlerFor wraps the proxy in the loop guard. Split from buildObjProxy so
// tests keep a typed handle on the ReverseProxy (to swap its Transport) while the
// served handler is the guarded one — ReverseProxy's Director cannot abort a
// request, so the check has to live in front of it.
func objProxyHandlerFor(rp *httputil.ReverseProxy, c *objProxyCounters) http.Handler {
	return &objProxyHandler{rp: rp, c: c}
}

// objProxyHandlerForHost is objProxyHandlerFor with the VIRTUAL-HOST guard armed.
// Separate constructor so existing callers/tests keep the old behaviour explicitly
// rather than acquiring a new rejection by accident.
func objProxyHandlerForHost(rp *httputil.ReverseProxy, c *objProxyCounters, upstream string) http.Handler {
	return &objProxyHandler{rp: rp, c: c, upstream: upstream}
}

// objProxyHandler refuses a request that has already been through this proxy, or
// that is addressed in a form this proxy cannot encrypt correctly.
type objProxyHandler struct {
	rp       *httputil.ReverseProxy
	c        *objProxyCounters
	upstream string
}

func (h *objProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(objProxyLoopHeader) != "" {
		h.c.loops.Add(1)
		// 508 rather than 502: this is not an upstream failure, it is a routing
		// mistake in THIS cluster, and the distinction is the whole diagnosis.
		http.Error(w, "obj-proxy: request already passed through this proxy — DNS is routing the "+
			"upstream endpoint back to the obj-proxy Service instead of to Linode. Check the DaemonSet "+
			"still sets dnsPolicy: Default so the pod resolves via the node's resolver.",
			http.StatusLoopDetected)
		return
	}
	// VIRTUAL-HOST-STYLE ADDRESSING IS REFUSED, not silently forwarded.
	//
	// This proxy assumes path-style (`/<bucket>/<key>`): injectSSEC reads the object
	// key out of the PATH, and a bucket-style request (`bucket.<endpoint>/<key>`)
	// puts the key somewhere else entirely — a single-segment key then looks like a
	// bucket-level operation and gets NO SSE-C headers at all, which writes the
	// object in PLAINTEXT while everything reports healthy.
	//
	// Today nothing reaches here that way: the CoreDNS rewrite is an exact-match on
	// the endpoint name, so `bucket.<endpoint>` is never routed to us. That is
	// exactly why this guard is worth having — broadening that rewrite to a suffix
	// or regex is a natural-looking improvement, and it would arm the silent-
	// plaintext path above. This makes that attempt fail loudly on the first request
	// instead.
	//
	// 421 Misdirected Request is the honest code: the request reached a server that
	// cannot serve it correctly.
	if h.upstream != "" && r.Host != "" && !strings.EqualFold(r.Host, h.upstream) {
		h.c.misdirected.Add(1)
		http.Error(w, "obj-proxy: request addressed to "+r.Host+" but this proxy fronts "+h.upstream+
			" and handles PATH-STYLE requests only. A virtual-host-style request (bucket.<endpoint>) cannot "+
			"be encrypted correctly here — its object key is not in the path — so it is refused rather than "+
			"forwarded unencrypted.", http.StatusMisdirectedRequest)
		return
	}
	h.rp.ServeHTTP(w, r)
}

// objProxyServingTLS builds the SERVER side of the proxy's TLS.
//
// GetCertificate re-reads the keypair PER HANDSHAKE. This is not a
// micro-optimisation in reverse — it is the difference between a proxy that keeps
// working and one that stops dead on a schedule.
//
// ListenAndServeTLS(certFile, keyFile) loads the keypair ONCE at startup. The
// serving Certificate is 90 days with renewBefore 30d, so cert-manager rotates the
// Secret at day 60 and the kubelet updates the mount — and a process that loaded at
// boot goes on presenting the ORIGINAL leaf until it expires at day 90, then fails
// every handshake.
//
// On this component that is a cluster-wide outage with a 90-day fuse: Loki stops
// flushing chunks and Harbor stops serving images, because the DNS rewrite sends
// their object-storage traffic here. And nothing restarts it — the liveness probe
// is on the separate PLAINTEXT health port, which keeps answering 200 while every
// TLS handshake fails. The pod stays Ready through the whole outage.
//
// reconcile.go's metrics listener already carries this exact reasoning; the first
// revision of this file did not apply it.
func objProxyServingTLS(certFile, keyFile string) *tls.Config {
	return &tls.Config{
		// Explicit rather than inherited. Recent Go defaults servers to 1.2, but this
		// is a component whose entire purpose is an auditable transport posture, and
		// "the language happened to default correctly" is not a statement anyone can
		// check against the manifest.
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("reload obj-proxy serving keypair: %w", err)
			}
			return &c, nil
		},
	}
}

func objProxyTransport(caFile string) (*http.Transport, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pool, err := objProxyCertPool(caFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Transport{
		TLSClientConfig: tlsCfg,
		// Image layers and log chunks are large and bursty; the stdlib defaults keep
		// too few idle connections for a shared gateway and reconnect constantly.
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}, nil
}

// assertNotSelf refuses to start when the upstream hostname resolves to an
// address this process is serving on.
//
// IT IS A NARROW CHECK AND NOT THE REAL PROTECTION — see objProxyLoopHeader. It
// catches only a DIRECT self-resolution; the likelier shape, cluster DNS resolving
// the endpoint to the obj-proxy Service, produces a ClusterIP that is not a local
// address and passes here. Kept because a boot-time failure with a clear message is
// worth having for the case it does cover, and costs one lookup.
//
// It fails CLOSED only on a positive match. A resolution error is reported as a
// warning rather than a hard stop: DNS can be briefly unavailable at boot, and
// refusing to start then would turn a transient resolver blip into an outage of
// every image pull.
func assertNotSelf(upstream string) error {
	ips, err := net.LookupHost(upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "obj-proxy: WARNING: could not resolve upstream %q to check for a proxy loop: %v\n", upstream, err)
		return nil
	}
	local, err := localAddrSet()
	if err != nil {
		fmt.Fprintf(os.Stderr, "obj-proxy: WARNING: could not enumerate local addresses: %v\n", err)
		return nil
	}
	for _, ip := range ips {
		if local[ip] {
			return fmt.Errorf(
				"upstream %q resolves to this pod's own address %s — requests would loop forever.\n"+
					"This is what `dnsPolicy: Default` in the DaemonSet prevents: with cluster DNS the\n"+
					"CoreDNS rewrite that sends clients here also sends the proxy here. Set dnsPolicy:\n"+
					"Default so the pod resolves the real endpoint via the node's resolver",
				upstream, ip)
		}
	}
	return nil
}

func localAddrSet() (map[string]bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			out[ipnet.IP.String()] = true
		}
	}
	return out, nil
}

// objProxyHealth serves the kubelet probe and a counter dump. Plaintext and
// separate from the S3 listener for the same reason the reconciler splits its
// ports: an httpGet probe cannot present a client certificate, and nothing here
// carries object data or key material.
func objProxyHealth(c *objProxyCounters) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "llz_objproxy_requests_total %d\n", c.total.Load())
		fmt.Fprintf(w, "llz_objproxy_ssec_injected_total %d\n", c.injected.Load())
		fmt.Fprintf(w, "llz_objproxy_passthrough_total %d\n", c.passed.Load())
		fmt.Fprintf(w, "llz_objproxy_copy_source_total %d\n", c.copySrc.Load())
		fmt.Fprintf(w, "llz_objproxy_upstream_errors_total %d\n", c.upstreamE.Load())
		// Non-zero means DNS is sending the proxy's own upstream back to it. Alert on
		// it: the requests fail closed, but every one of them is a write that did not
		// happen.
		fmt.Fprintf(w, "llz_objproxy_loops_detected_total %d\n", c.loops.Load())
		// #397: how many requests arrived in the framing Linode rejects and were
		// repaired, and how many could not be. A climbing resign_failed_total means
		// writes are reaching the upstream in a form it will refuse.
		fmt.Fprintf(w, "llz_objproxy_resigned_total %d\n", c.resigned.Load())
		fmt.Fprintf(w, "llz_objproxy_resign_failed_total %d\n", c.resignErr.Load())
		fmt.Fprintf(w, "llz_objproxy_misdirected_total %d\n", c.misdirected.Load())
	})
	return mux
}

// objProxyCertPool loads a CA bundle for verifying the upstream. Kept local so the
// proxy has no dependency on the OpenBao client.
func objProxyCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read upstream CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("upstream CA %s contains no valid certificate", path)
	}
	return pool, nil
}

// upstreamHostOnly strips any scheme or trailing slash an operator may have pasted
// into --upstream, so `https://us-ord-10.linodeobjects.com/` and the bare host
// behave identically instead of producing an unresolvable name.
func upstreamHostOnly(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	return strings.TrimSuffix(s, "/")
}
