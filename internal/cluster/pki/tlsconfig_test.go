package pki

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// handshake runs a TLS handshake between server and client over loopback
// and one byte of data from the server, so that a client certificate the
// server refuses after the client's side of the handshake is done fails the
// client too. It returns the server's state of a connection it accepted and
// what each side saw.
func handshake(t *testing.T, server, client *tls.Config) (state *tls.ConnectionState, serverErr, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	deadline := time.Now().Add(10 * time.Second)

	type result struct {
		err   error
		state *tls.ConnectionState
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() { done <- r }()
		conn, err := ln.Accept()
		if err != nil {
			r.err = err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(deadline)
		s := tls.Server(conn, server)
		if r.err = s.Handshake(); r.err != nil {
			return
		}
		cs := s.ConnectionState()
		r.state = &cs
		_, r.err = s.Write([]byte{1})
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, conn.SetDeadline(deadline))
	c := tls.Client(conn, client)
	clientErr = c.Handshake()
	if clientErr == nil {
		_, clientErr = c.Read(make([]byte, 1))
	}
	_ = conn.Close()
	r := <-done
	return r.state, r.err, clientErr
}

type tlsFixture struct {
	ca         CA
	serverName string
	server     tls.Certificate
	serverCert *x509.Certificate
	serverKey  *ecdsa.PrivateKey
	client     tls.Certificate
	clientCert *x509.Certificate
	clientKey  *ecdsa.PrivateKey
}

func newTLSFixture(t *testing.T) tlsFixture {
	t.Helper()
	now := time.Now()
	ca := newCA(t, now)
	name := NodeServerName(testInstall, "node1")
	srv, srvKey := issueServer(t, ca, name, now)
	cli, cliKey := issueClient(t, ca, hostID("node2"), now)
	return tlsFixture{
		ca:         ca,
		serverName: name,
		server:     TLSCertificate(srvKey, srv, ca.Cert),
		serverCert: srv,
		serverKey:  srvKey,
		client:     TLSCertificate(cliKey, cli),
		clientCert: cli,
		clientKey:  cliKey,
	}
}

func TestMutualTLS(t *testing.T) {
	f := newTLSFixture(t)
	state, serverErr, clientErr := handshake(t,
		ServerTLS(f.server, poolOf(f.ca)),
		ClientTLS(f.client, f.ca.SHA256(), f.serverName))
	require.NoError(t, serverErr)
	require.NoError(t, clientErr)
	require.Equal(t, f.clientCert.Raw, state.PeerCertificates[0].Raw)
	id, err := PeerIdentity(state)
	require.NoError(t, err)
	require.Equal(t, hostID("node2"), id)
}

func TestPeerIdentityRefusesAConnectionWithoutACertificate(t *testing.T) {
	f := newTLSFixture(t)
	state, serverErr, clientErr := handshake(t,
		ServerTLS(f.server, poolOf(f.ca)),
		ClientTLS(tls.Certificate{}, f.ca.SHA256(), f.serverName))
	require.NoError(t, serverErr, "the join comes without a certificate")
	require.NoError(t, clientErr)
	_, err := PeerIdentity(state)
	require.ErrorIs(t, err, ErrNoClientCert)

	_, err = PeerIdentity(nil)
	require.ErrorIs(t, err, ErrNoClientCert, "a request that did not come over TLS")

	_, err = PeerIdentity(&tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.clientCert}})
	require.ErrorIs(t, err, ErrNoClientCert, "a certificate is not taken unless it was verified")

	_, err = PeerIdentity(&tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{f.serverCert, f.ca.Cert}}})
	require.ErrorContains(t, err, "carries no pco identity")
}

func TestTLSConfigsSpeakOnlyTLS13(t *testing.T) {
	f := newTLSFixture(t)
	for _, c := range []*tls.Config{ServerTLS(f.server, poolOf(f.ca)), ClientTLS(f.client, f.ca.SHA256(), f.serverName)} {
		require.Equal(t, uint16(tls.VersionTLS13), c.MinVersion)
	}
}

func TestClientTLSPinsTheCA(t *testing.T) {
	f := newTLSFixture(t)
	now := time.Now()

	other := newCA(t, now)
	otherSrv, otherKey := issueServer(t, other, f.serverName, now)
	wrongName, wrongNameKey := issueServer(t, f.ca, NodeServerName(testInstall, "node3"), now)
	controller, controllerKey := issueServer(t, f.ca, ControllerServerName(testInstall), now)

	cases := []struct {
		name          string
		server        tls.Certificate
		pin           string
		wantSubstring string
	}{
		{"the right name under another CA", TLSCertificate(otherKey, otherSrv, other.Cert), f.ca.SHA256(), "not under the CA"},
		{"another CA sending ours along", TLSCertificate(otherKey, otherSrv, f.ca.Cert), f.ca.SHA256(), "verifying the server's certificate"},
		{"the right CA with another name", TLSCertificate(wrongNameKey, wrongName, f.ca.Cert), f.ca.SHA256(), "node-node3"},
		{"the controller's name to a node", TLSCertificate(controllerKey, controller, f.ca.Cert), f.ca.SHA256(), "controller.pco"},
		{"the right CA left out of the chain", TLSCertificate(f.serverKey, f.serverCert), f.ca.SHA256(), "not under the CA"},
		{"a client certificate serving", TLSCertificate(f.clientKey, f.clientCert, f.ca.Cert), f.ca.SHA256(), "verifying the server's certificate"},
		{"an empty pin", f.server, "", "not under the CA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, clientErr := handshake(t,
				ServerTLS(c.server, poolOf(f.ca)),
				ClientTLS(f.client, c.pin, f.serverName))
			require.ErrorContains(t, clientErr, c.wantSubstring)
		})
	}

	t.Run("an upper-case pin", func(t *testing.T) {
		_, _, clientErr := handshake(t,
			ServerTLS(f.server, poolOf(f.ca)),
			ClientTLS(f.client, strings.ToUpper(f.ca.SHA256()), f.serverName))
		require.NoError(t, clientErr)
	})
}

func TestServerTLSRefuses(t *testing.T) {
	f := newTLSFixture(t)
	now := time.Now()
	other := newCA(t, now)
	otherCli, otherKey := issueClient(t, other, hostID("node2"), now)
	srvAsClient, srvAsClientKey := issueServer(t, f.ca, NodeServerName(testInstall, "node2"), now)

	tls12 := ClientTLS(f.client, f.ca.SHA256(), f.serverName)
	tls12.MinVersion, tls12.MaxVersion = tls.VersionTLS12, tls.VersionTLS12

	cases := []struct {
		name          string
		client        *tls.Config
		pool          *x509.CertPool
		wantSubstring string
	}{
		{"TLS 1.2", tls12, poolOf(f.ca), "unsupported versions"},
		{"a client under another CA", ClientTLS(TLSCertificate(otherKey, otherCli), f.ca.SHA256(), f.serverName), poolOf(f.ca), "unknown authority"},
		{"a server certificate as a client's", ClientTLS(TLSCertificate(srvAsClientKey, srvAsClient), f.ca.SHA256(), f.serverName), poolOf(f.ca), "usage"},
		{"no pool", ClientTLS(f.client, f.ca.SHA256(), f.serverName), nil, "unknown authority"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, serverErr, _ := handshake(t, ServerTLS(f.server, c.pool), c.client)
			require.ErrorContains(t, serverErr, c.wantSubstring)
		})
	}
}

func TestServerTLSTakesBothCAsOfARotation(t *testing.T) {
	f := newTLSFixture(t)
	now := time.Now()
	next := newCA(t, now)
	nextCli, nextKey := issueClient(t, next, hostID("node3"), now)

	cases := []struct {
		name   string
		client tls.Certificate
	}{
		{"the previous CA", f.client},
		{"the new CA", TLSCertificate(nextKey, nextCli)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, serverErr, clientErr := handshake(t,
				ServerTLS(f.server, poolOf(f.ca, next)),
				ClientTLS(c.client, f.ca.SHA256(), f.serverName))
			require.NoError(t, serverErr)
			require.NoError(t, clientErr)
			_, err := PeerIdentity(state)
			require.NoError(t, err)
		})
	}
}

func TestServerTLSResumesNoSession(t *testing.T) {
	f := newTLSFixture(t)
	require.True(t, ServerTLS(f.server, poolOf(f.ca)).SessionTicketsDisabled)
}
