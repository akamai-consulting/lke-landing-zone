package assertobs

// ci_loki_prove_writes.go — make "Loki persists logs" a PROVEN property rather than
// an inferred one.
//
// WHY THIS EXISTS. assert-loki's other checks are all observational, and every one
// of them can be configreadiness.Satisfied by a Loki that has never written a byte. #397 is exactly
// that state: Running, Ready, Synced, Healthy, S3-configured, and 403 AccessDenied on
// every PutObject. Two successive attempts to catch it observationally both passed
// vacuously on a real cluster — a 60s flush-error scan finds nothing when nothing has
// been ATTEMPTED, and "has anything reached the bucket" cannot fail on a young
// cluster because Loki legitimately holds its first chunk until chunk_idle_period.
//
// The way out is the one the Harbor push already uses: stop waiting for the system
// to do the thing, and MAKE it do the thing. Loki's ingesters expose /flush, which
// writes their in-memory chunks to object storage on demand. Trigger it, then look
// for the object. That collapses "we did not see a failure" into "we saw it
// succeed", which is a different claim entirely.
//
// COST OF BEING WRONG, both ways. Flushing forces smaller chunks than Loki would
// choose, so this only fires when the cheap evidence is absent — on a cluster that
// has already written since its ingesters came up, nothing here runs at all. And it
// is opt-out (--no-flush-probe) for anyone who wants a strictly read-only gate.

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/objstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/portfwd"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
)

const (
	// lokiFlushPath is the ingester admin endpoint that writes in-memory chunks to
	// object storage. Documented as "mainly used for local testing", which is what
	// this is: a test, run against a live cluster, that needs a write to observe.
	lokiFlushPath = "/flush"
	// lokiDefaultHTTPPort is Loki's http_listen_port. Used only when the pod spec
	// does not name a port — read from the pod first so a cluster that moved it is
	// still probed correctly rather than silently timing out on the wrong one.
	lokiDefaultHTTPPort = "3100"
)

// lokiProveBudget is how long to wait for a flushed chunk to appear in the bucket
// after the trigger. Flushes are asynchronous and the object has to be listable, so
// this is a poll, not a sleep. Generous enough for a loaded bootstrap, short enough
// that a genuine write outage does not stall the lane.
var (
	lokiProveBudget   = 90 * time.Second
	lokiProveInterval = 5 * time.Second
)

// lokiProveWrites returns a verdict on whether Loki can actually persist a chunk.
//
// Three outcomes, and the middle one is the point: an existing post-start object is
// already proof and costs nothing; otherwise the flush makes proof obtainable; and
// only a flush that produces nothing is a failure.
func lokiProveWrites(nameMatch, region string, allowFlush bool) []lokiWriteMsg {
	bucket, endpoint := lokiChunksTarget(region)
	if bucket == "" || endpoint == "" {
		return []lokiWriteMsg{{"SKIP: no chunks bucket/endpoint for this deployment (--region given? spec readable?) — the write path is unmeasured here", false}}
	}
	ak, sk, err := objenc.ObjEncConsumerCreds(caps.ObjEncDeps(), objenc.LokiObjSecretRef, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
	if err != nil {
		return []lokiWriteMsg{{"SKIP: could not read " + objenc.LokiObjSecretRef + " (" + err.Error() + ") — the write path is unmeasured here", false}}
	}

	// Cheap evidence first. A cluster that has written since its ingesters started
	// is already proven, and forcing a flush there would be a gratuitous write.
	start, serr := lokiOldestIngesterStart(nameMatch)
	if serr == nil {
		if newest, ok := lokiNewestObject(ak, sk, endpoint, bucket); ok && newest.After(start) {
			return []lokiWriteMsg{{fmt.Sprintf(
				"PROVEN: Loki has persisted to object storage since the ingesters started (newest object %s)",
				newest.Format(time.RFC3339)), false}}
		}
	}

	if !allowFlush {
		return []lokiWriteMsg{{"UNPROVEN: nothing written since the ingesters started and --no-flush-probe was given, " +
			"so nothing asked Loki to write. This lane did not confirm Loki persists anything", false}}
	}

	ingesters := lokiIngesterPods(nameMatch)
	if len(ingesters) == 0 {
		return []lokiWriteMsg{{"SKIP: no ingester pods to flush — the write path is unmeasured here", false}}
	}

	// The trigger instant is the reference: any chunk that lands after it is one this
	// flush caused, not a leftover. Read the clock BEFORE the trigger so a slow flush
	// cannot produce an object that looks older than the request that caused it.
	triggered := lokiNow()
	var flushed int
	var flushErrs []string
	for _, p := range ingesters {
		if err := lokiFlushIngester(p); err != nil {
			flushErrs = append(flushErrs, fmt.Sprintf("%s/%s: %v", p.ns, p.name, err))
			continue
		}
		flushed++
	}
	if flushed == 0 {
		return []lokiWriteMsg{{"SKIP: could not reach /flush on any ingester (" + strings.Join(flushErrs, "; ") +
			") — the write path is unmeasured here, which is not the same as broken", false}}
	}

	out := []lokiWriteMsg{{fmt.Sprintf("flushed %d/%d ingester(s) to force a write to %s", flushed, len(ingesters), bucket), false}}
	for _, e := range flushErrs {
		out = append(out, lokiWriteMsg{"  note: " + e, false})
	}

	deadline := lokiNow().Add(lokiProveBudget)
	var landed bool
	var landedAfter time.Duration
	for {
		if newest, ok := lokiNewestObject(ak, sk, endpoint, bucket); ok && newest.After(triggered) {
			landed, landedAfter = true, newest.Sub(triggered).Round(time.Second)
			break
		}
		if !lokiNow().Before(deadline) {
			break
		}
		lokiProveSleep(lokiProveInterval)
	}

	// ERRORS ARE CHECKED WHETHER OR NOT SOMETHING LANDED, and that ordering is the
	// point. An earlier version returned PROVEN the moment any object appeared, which
	// meant a fleet where two ingesters wrote and a third could not reported success
	// — a third of the cluster's logs lost, with the failing replica's 403s never
	// read. That is the PARTIAL failure this gate was written to surface, and it was
	// producing it instead: lokiFlushIngester deliberately flushes every ingester
	// rather than one via a Service, precisely so a broken replica cannot hide, and
	// the verdict then threw that away.
	errs := lokiFlushFailuresSince(nameMatch, triggered)

	switch {
	case len(errs) > 0 && landed:
		return append(out,
			lokiWriteMsg{fmt.Sprintf("FAIL: a chunk reached %s, but %d ingester write error(s) were logged doing it — "+
				"SOME replicas cannot persist, so a share of this cluster's logs is being dropped while the "+
				"write path looks healthy", bucket, len(errs)), true},
			lokiWriteMsg{"  " + errs[0], false})
	case len(errs) > 0:
		return append(out,
			lokiWriteMsg{fmt.Sprintf("FAIL: %d ingester(s) flushed, nothing reached %s within %s, and they logged "+
				"write errors doing it — Loki is not persisting logs", flushed, bucket, lokiProveBudget), true},
			lokiWriteMsg{"  " + errs[0], false},
			lokiWriteMsg{"  Every other signal in this lane can be green in this state — that is what #397 is. " +
				"A 403 AccessDenied here is usually NOT the credential but Linode's Ceph rejecting the AWS SDK's " +
				"default aws-chunked trailer-checksum framing", false})
	case landed:
		return append(out, lokiWriteMsg{fmt.Sprintf(
			"PROVEN: a chunk reached object storage %s after the flush, with no ingester reporting a write "+
				"error — Loki's write path works end to end", landedAfter), false})
	}
	// Nothing landed and nothing complained. A flush cannot invent data, so on a
	// quiet cluster this is normal: the ingesters had no buffered chunk to write.
	// Failing here would color.Red a healthy Loki for having nothing to say.
	return append(out, lokiWriteMsg{fmt.Sprintf(
		"INCONCLUSIVE: %d ingester(s) flushed without error and nothing new reached %s within %s — they had no "+
			"buffered chunk to write, so this neither proves nor disproves the write path. On a cluster with log "+
			"traffic that is unexpected; check the distributors are reaching the ingesters at all",
		flushed, bucket, lokiProveBudget), false})
}

// lokiFlushFailuresSince returns flush errors logged after t. The window is derived
// from elapsed time so a long-running gate does not re-read an entire pod log, and
// so an error from BEFORE the trigger cannot be blamed on this flush.
func lokiFlushFailuresSince(nameMatch string, t time.Time) []string {
	window := lokiNow().Sub(t) + 5*time.Second
	var out []string
	for _, p := range lokiIngesterPods(nameMatch) {
		raw, err := lokiLogs(p.ns, p.name, window)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(raw, "\n") {
			if strings.Contains(line, "failed to flush") {
				out = append(out, strings.TrimSpace(harborauth.TruncateForError([]byte(p.ns+"/"+p.name+": "+line))))
			}
		}
	}
	return out
}

// lokiIngesterPods is the subset that holds chunks. Only ingesters flush; queriers
// and gateways have nothing to write.
func lokiIngesterPods(nameMatch string) []lokiPod {
	var out []lokiPod
	for _, p := range lokiPodsFn(nameMatch) {
		if strings.Contains(p.name, "ingester") {
			out = append(out, p)
		}
	}
	return out
}

// lokiNewestObject returns the newest object in the bucket. objstore.SampleObjectKeys sorts
// newest-first, so one key is enough.
var lokiNewestObject = func(ak, sk, endpoint, bucket string) (time.Time, bool) {
	refs, err := objstore.SampleObjectKeys(ak, sk, endpoint, bucket, 1)
	if err != nil || len(refs) == 0 {
		return time.Time{}, false
	}
	return refs[0].LastModified, true
}

var lokiProveSleep = time.Sleep

// lokiFlushIngester POSTs /flush to one ingester through an ephemeral port-forward.
//
// PER POD, not through a Service: /flush is ingester-local, so a Service would
// load-balance the trigger to one arbitrary replica and leave the rest holding their
// chunks. Proving the write path on one ingester while another silently cannot write
// is the partial failure this gate is supposed to surface, not produce.
var lokiFlushIngester = func(p lokiPod) error {
	port := lokiIngesterHTTPPort(p)
	// ":0" → kubectl picks a free local port and announces it, so lanes running in
	// parallel cannot collide on a fixed one.
	cmd := exec.Command("kubectl", "port-forward", "-n", p.ns, "pod/"+p.name, ":"+port)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("kubectl port-forward: %w", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	local, err := portfwd.ReadForwardPortTimeout(stdout, portfwd.ForwardEstablishTimeout)
	if err != nil {
		return err
	}
	// Keep draining: once nobody reads, kubectl's per-connection logging fills the
	// pipe buffer and blocks its writer mid-forward.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	client := &http.Client{Timeout: 30 * time.Second}
	base := "http://127.0.0.1:" + local
	if err := warmUpForward(client, base+"/ready"); err != nil {
		return err
	}
	resp, err := client.Post(base+lokiFlushPath, "text/plain", nil)
	if err != nil {
		return fmt.Errorf("POST %s: %w", lokiFlushPath, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s returned HTTP %d: %s", lokiFlushPath, resp.StatusCode, harborauth.TruncateForError(body))
	}
	return nil
}

// lokiIngesterHTTPPort reads the pod's own HTTP container port, falling back to
// Loki's default. Reading it means a cluster that moved the port is still probed
// rather than timing out against a port nothing listens on — and a timeout would be
// reported as "unmeasured", quietly costing the proof this file exists to provide.
func lokiIngesterHTTPPort(p lokiPod) string {
	out, err := caps.KubectlOut("-n", p.ns, "get", "pod", p.name, "-o",
		`jsonpath={.spec.containers[*].ports[?(@.name=="http-metrics")].containerPort}`)
	if err == nil {
		if port := strings.Fields(strings.TrimSpace(out)); len(port) > 0 {
			return port[0]
		}
	}
	out, err = caps.KubectlOut("-n", p.ns, "get", "pod", p.name, "-o",
		`jsonpath={.spec.containers[*].ports[?(@.name=="http")].containerPort}`)
	if err == nil {
		if port := strings.Fields(strings.TrimSpace(out)); len(port) > 0 {
			return port[0]
		}
	}
	return lokiDefaultHTTPPort
}
