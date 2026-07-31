package main

// pg_probe.go — enough of the PostgreSQL wire protocol to answer one question:
// does this server ACCEPT this credential?
//
// There is no Postgres driver in this module and this does not add one. The
// module is deliberately stdlib-first, and the question does not need a driver:
// it needs the TLS negotiation, the startup message, one authentication
// exchange, and the server's verdict. No queries are ever sent.
//
// WHY NOT JUST DIAL THE PORT. A TCP connect proves DNS, routing and the
// firewall — none of which is what breaks here. `rotate-db-admin` resets the
// Managed Postgres password IN PLACE with no overlap window (which is why it is
// on-demand and not on a cron), so the failure mode is a live endpoint that
// answers cheerfully and rejects the credential every consumer holds. Only
// completing authentication separates those.
//
// SCRAM-SHA-256 is implemented because it is what Managed Postgres actually
// negotiates; MD5 and trust are handled because a server configured either way
// should not read as a failure. The SCRAM chain is unit-tested against RFC 7677's
// published vector, so the crypto is pinned to the spec rather than to whatever
// this file happens to do.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// pgVerdict is what the probe learned.
type pgVerdict string

const (
	pgAuthenticated pgVerdict = "authenticated" // the server accepted the credential
	pgRejected      pgVerdict = "rejected"      // the server answered and refused it
	pgUnreachable   pgVerdict = "unreachable"   // never got far enough to ask
)

// ── SCRAM-SHA-256 (RFC 5802 / RFC 7677) ─────────────────────────────────────

func hmac256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// scramClientProof computes the ClientProof for a SCRAM-SHA-256 exchange. Pure,
// and the piece the RFC vector pins.
func scramClientProof(password, salt string, iterations int, authMessage string) string {
	salted := pbkdf2.Key([]byte(password), []byte(salt), iterations, sha256.Size, sha256.New)
	clientKey := hmac256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	sig := hmac256(storedKey[:], authMessage)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ sig[i]
	}
	return base64.StdEncoding.EncodeToString(proof)
}

// parseScramServerFirst pulls r=<nonce>, s=<salt b64>, i=<iterations> out of a
// server-first message. Pure.
func parseScramServerFirst(msg string) (nonce, salt string, iterations int, err error) {
	for _, part := range strings.Split(msg, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "r":
			nonce = v
		case "s":
			raw, derr := base64.StdEncoding.DecodeString(v)
			if derr != nil {
				return "", "", 0, fmt.Errorf("SCRAM salt is not base64: %w", derr)
			}
			salt = string(raw)
		case "i":
			iterations, err = strconv.Atoi(v)
			if err != nil {
				return "", "", 0, fmt.Errorf("SCRAM iteration count %q is not a number", v)
			}
		}
	}
	if nonce == "" || salt == "" || iterations <= 0 {
		return "", "", 0, fmt.Errorf("incomplete SCRAM server-first message %q", msg)
	}
	return nonce, salt, iterations, nil
}

// ── wire framing ────────────────────────────────────────────────────────────

// pgMessage writes a typed frontend message.
func pgMessage(typ byte, body []byte) []byte {
	out := make([]byte, 0, 5+len(body))
	out = append(out, typ)
	out = binary.BigEndian.AppendUint32(out, uint32(4+len(body)))
	return append(out, body...)
}

// readPGMessage reads one backend message: its type byte and body.
func readPGMessage(r io.Reader) (byte, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n < 4 || n > 1<<20 {
		return 0, nil, fmt.Errorf("implausible Postgres message length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

// pgErrorText pulls the human-readable message out of an ErrorResponse body.
func pgErrorText(body []byte) string {
	var parts []string
	for i := 0; i < len(body); {
		f := body[i]
		if f == 0 {
			break
		}
		j := i + 1
		for j < len(body) && body[j] != 0 {
			j++
		}
		if f == 'M' || f == 'C' {
			parts = append(parts, string(body[i+1:j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ": ")
}

// pgDial opens a TLS connection to a Postgres endpoint. Seamed for tests.
var pgDial = func(addr, serverName string, timeout time.Duration) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	// SSLRequest: the magic 80877103, then one byte back — 'S' means proceed.
	req := binary.BigEndian.AppendUint32(nil, 8)
	req = binary.BigEndian.AppendUint32(req, 80877103)
	if _, err := raw.Write(req); err != nil {
		raw.Close()
		return nil, err
	}
	resp := make([]byte, 1)
	if _, err := io.ReadFull(raw, resp); err != nil {
		raw.Close()
		return nil, err
	}
	if resp[0] != 'S' {
		raw.Close()
		return nil, fmt.Errorf("the server refused TLS (answered %q) — Managed Postgres requires sslmode=require", resp[0])
	}
	tconn := tls.Client(raw, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
	if err := tconn.Handshake(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tconn, nil
}

// pgAuthenticate runs the startup + authentication exchange and reports whether
// the credential was accepted. It never issues a query.
func pgAuthenticate(conn net.Conn, user, password, database string, timeout time.Duration) (pgVerdict, string) {
	_ = conn.SetDeadline(time.Now().Add(timeout))

	startup := binary.BigEndian.AppendUint32(nil, 196608) // protocol 3.0
	for _, kv := range [][2]string{{"user", user}, {"database", database}} {
		startup = append(startup, kv[0]...)
		startup = append(startup, 0)
		startup = append(startup, kv[1]...)
		startup = append(startup, 0)
	}
	startup = append(startup, 0)
	framed := binary.BigEndian.AppendUint32(nil, uint32(len(startup)+4))
	if _, err := conn.Write(append(framed, startup...)); err != nil {
		return pgUnreachable, "writing the startup message: " + err.Error()
	}

	for {
		typ, body, err := readPGMessage(conn)
		if err != nil {
			return pgUnreachable, "reading the server's reply: " + err.Error()
		}
		switch typ {
		case 'E': // ErrorResponse
			return pgRejected, pgErrorText(body)
		case 'Z': // ReadyForQuery — fully authenticated
			return pgAuthenticated, "server reached ReadyForQuery"
		case 'R': // Authentication*
			if len(body) < 4 {
				return pgUnreachable, "truncated authentication message"
			}
			switch binary.BigEndian.Uint32(body[:4]) {
			case 0: // AuthenticationOk
				continue // wait for ReadyForQuery
			case 10: // SASL
				v, msg := pgSCRAM(conn, body[4:], password, timeout)
				if v != pgAuthenticated {
					return v, msg
				}
				continue
			case 5: // MD5 — unsupported here, but the server ANSWERED
				return pgUnreachable, "the server negotiated MD5 authentication, which this probe does not implement " +
					"(Managed Postgres uses SCRAM-SHA-256); the endpoint and TLS are fine"
			default:
				return pgUnreachable, "the server requested an authentication method this probe does not implement"
			}
		default:
			// ParameterStatus / BackendKeyData arrive after auth succeeds.
			continue
		}
	}
}

// pgSCRAM performs the SASL exchange.
func pgSCRAM(conn net.Conn, mechanisms []byte, password string, timeout time.Duration) (pgVerdict, string) {
	if !strings.Contains(string(mechanisms), "SCRAM-SHA-256") {
		return pgUnreachable, "the server offered no SCRAM-SHA-256 mechanism"
	}
	// A fixed client nonce is fine: SCRAM's replay protection is the SERVER's
	// nonce, and this probe authenticates once over a fresh TLS session.
	const cnonce = "llzprobeclientnonce"
	clientFirstBare := "n=,r=" + cnonce
	initial := "n,," + clientFirstBare

	body := append([]byte("SCRAM-SHA-256"), 0)
	body = binary.BigEndian.AppendUint32(body, uint32(len(initial)))
	body = append(body, initial...)
	if _, err := conn.Write(pgMessage('p', body)); err != nil {
		return pgUnreachable, "writing SASLInitialResponse: " + err.Error()
	}

	typ, rbody, err := readPGMessage(conn)
	if err != nil {
		return pgUnreachable, "reading the SCRAM challenge: " + err.Error()
	}
	if typ == 'E' {
		return pgRejected, pgErrorText(rbody)
	}
	if typ != 'R' || len(rbody) < 4 || binary.BigEndian.Uint32(rbody[:4]) != 11 {
		return pgUnreachable, "unexpected reply to SASLInitialResponse"
	}
	serverFirst := string(rbody[4:])
	nonce, salt, iters, perr := parseScramServerFirst(serverFirst)
	if perr != nil {
		return pgUnreachable, perr.Error()
	}

	finalNoProof := "c=biws,r=" + nonce
	authMessage := clientFirstBare + "," + serverFirst + "," + finalNoProof
	final := finalNoProof + ",p=" + scramClientProof(password, salt, iters, authMessage)
	if _, err := conn.Write(pgMessage('p', []byte(final))); err != nil {
		return pgUnreachable, "writing the SCRAM final message: " + err.Error()
	}

	typ, rbody, err = readPGMessage(conn)
	if err != nil {
		return pgUnreachable, "reading the SCRAM result: " + err.Error()
	}
	if typ == 'E' {
		// THE case this exists for: the server answered and refused the password.
		return pgRejected, pgErrorText(rbody)
	}
	return pgAuthenticated, "SCRAM-SHA-256 accepted"
}

// pgProbeCredential is the whole probe: TLS, startup, auth, verdict.
var pgProbeCredential = func(host, port, user, password, database string, timeout time.Duration) (pgVerdict, string) {
	conn, err := pgDial(net.JoinHostPort(host, port), host, timeout)
	if err != nil {
		return pgUnreachable, err.Error()
	}
	defer conn.Close()
	return pgAuthenticate(conn, user, password, database, timeout)
}
