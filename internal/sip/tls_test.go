package sip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"
)

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sip.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// SIP over TLS (RFC 3261 section 26): requests and replies over a TLS connection carry the "tls" transport, the Via
// says TLS, the server can send over the connection the client opened, and a client that is not speaking TLS is
// refused at the handshake.
func TestTransactionsOverTLS(t *testing.T) {
	srvRec := &recorder{reqs: make(chan *ServerTx, 10), acks: make(chan *Message, 10)}
	server := newTestStack(t, srvRec, nil)
	cliRec := &recorder{reqs: make(chan *ServerTx, 10), acks: make(chan *Message, 10)}
	client := newTLSClientStack(t, cliRec, true) // the test server's certificate is self-signed

	if server.TLSPort() != 0 {
		t.Fatal("no TLS port before AddTLSListener")
	}
	port := freePort(t)
	cfg := &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, MinVersion: tls.VersionTLS12}
	if err := server.AddTLSListener("127.0.0.1", port, cfg); err != nil {
		t.Fatal(err)
	}
	if server.TLSPort() != port {
		t.Fatalf("TLSPort %d, want %d", server.TLSPort(), port)
	}
	if err := server.AddTLSListener("127.0.0.1", port, nil); err == nil {
		t.Fatal("a TLS listener without a certificate should be refused")
	}
	dst := Addr{Transport: "tls", AP: mustAP(t, "127.0.0.1", port)}

	tx, err := client.Request(buildReq("OPTIONS", server, 1), dst)
	if err != nil {
		t.Fatal(err)
	}
	stx := <-srvRec.reqs
	if stx.Request.Src.Transport != "tls" {
		t.Fatalf("request arrived as %q, want tls", stx.Request.Src.Transport)
	}
	if via, err := stx.Request.TopVia(); err != nil || via.Transport != "TLS" {
		t.Fatalf("Via %+v %v, want transport TLS", via, err)
	}
	if err := stx.Respond(NewResponse(stx.Request, 200, "")); err != nil {
		t.Fatal(err)
	}
	if resp := <-tx.Responses(); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %v", resp)
	}

	// The server reaches the client over the connection the client opened.
	back, err := server.Request(buildReq("OPTIONS", client, 2), Addr{Transport: "tls", AP: stx.Request.Src.AP})
	if err != nil {
		t.Fatal(err)
	}
	ctx := <-cliRec.reqs
	if ctx.Request.Src.Transport != "tls" {
		t.Fatalf("server request arrived as %q", ctx.Request.Src.Transport)
	}
	_ = ctx.Respond(NewResponse(ctx.Request, 200, ""))
	if resp := <-back.Responses(); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("server to client: %v", resp)
	}

	// Plain TCP to the TLS port gets nowhere: no SIP message reaches the handler.
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("OPTIONS sip:x@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/TCP 127.0.0.1;branch=z9hG4bKplain\r\nCall-ID: p\r\nCSeq: 1 OPTIONS\r\nFrom: <sip:a@x>;tag=1\r\nTo: <sip:x@x>\r\nContent-Length: 0\r\n\r\n"))
	select {
	case m := <-srvRec.reqs:
		t.Fatalf("a plain-text message got through the TLS port: %s", m.Request.Short())
	case <-time.After(300 * time.Millisecond):
	}
	c.Close()
}


// newTLSClientStack is newTestStack with the TLS certificate check of destinations switched on or off.
func newTLSClientStack(t *testing.T, h Handler, noVerify bool) *Stack {
	t.Helper()
	var st *Stack
	cfg := Config{
		Transport: TransportConfig{ListenIP: "127.0.0.1", EnableTCP: true, TLSNoVerify: noVerify},
		Advertise: func(Addr) (string, int) { return "127.0.0.1", st.Port() },
		T1:        50 * time.Millisecond,
	}
	st, err := NewStack(cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// By default the certificate of a TLS destination is checked: a server with a self-signed certificate is not
// reached, and nothing is sent to it. (TLSNoVerify, from sip-tls-verify=false, is what TestTransactionsOverTLS uses.)
func TestTLSDestinationCertificateIsVerifiedByDefault(t *testing.T) {
	srvRec := &recorder{reqs: make(chan *ServerTx, 10), acks: make(chan *Message, 10)}
	server := newTestStack(t, srvRec, nil)
	port := freePort(t)
	if err := server.AddTLSListener("127.0.0.1", port, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatal(err)
	}
	client := newTLSClientStack(t, &recorder{reqs: make(chan *ServerTx, 10), acks: make(chan *Message, 10)}, false)
	_, err := client.Request(buildReq("OPTIONS", server, 1), Addr{Transport: "tls", AP: mustAP(t, "127.0.0.1", port)})
	if err == nil {
		t.Fatal("a self-signed server certificate should be refused by default")
	}
	select {
	case m := <-srvRec.reqs:
		t.Fatalf("a request reached the server: %s", m.Request.Short())
	case <-time.After(300 * time.Millisecond):
	}
}
