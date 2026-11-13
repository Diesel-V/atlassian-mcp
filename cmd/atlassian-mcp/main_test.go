package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atlassian-mcp/internal/config"
	"atlassian-mcp/internal/store"
	"atlassian-mcp/internal/store/memory"
)

func TestAgentCLI(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runAgentCmd(ctx, st, args, strings.NewReader(stdin), &out)
		return out.String(), err
	}

	if _, err := run("", "add", "-client-id", "bot", "-name", "Tickets", "-owner", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("", "add", "-client-id", "bot", "-name", "x", "-owner", "y"); err == nil {
		t.Fatal("duplicate agent accepted")
	}
	if _, err := run("pat-123\n", "set-token", "-client-id", "bot", "-provider", "jira"); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAgent(ctx, "bot")
	if a.Credentials[store.Jira] != "pat-123" || !a.Enabled {
		t.Fatalf("agent = %+v", a)
	}

	out, err := run("", "list")
	if err != nil || !strings.Contains(out, "bot") || !strings.Contains(out, "jira") || strings.Contains(out, "pat-123") {
		t.Fatalf("list output (must not show tokens):\n%s %v", out, err)
	}

	if _, err := run("", "disable", "-client-id", "bot"); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAgent(ctx, "bot"); a.Enabled {
		t.Fatal("not disabled")
	}
	if _, err := run("", "remove-token", "-client-id", "bot", "-provider", "jira"); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAgent(ctx, "bot"); a.Credentials[store.Jira] != "" {
		t.Fatal("token not removed")
	}

	for _, args := range [][]string{
		{"set-token", "-client-id", "bot", "-provider", "gitlab"},
		{"set-token", "-client-id", "ghost", "-provider", "jira"},
		{"enable"},
		{"frobnicate", "-client-id", "bot"},
	} {
		if _, err := run("x", args...); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
	if _, err := run("  ", "set-token", "-client-id", "bot", "-provider", "jira"); err == nil {
		t.Error("empty token accepted")
	}
}

func TestUserOffboardCLI(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	st := memory.New()
	_ = st.CreateAPIKey(ctx, &store.APIKey{ID: "k1", Username: "leaver", Name: "oc", SecretHash: "h", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	_ = st.PutToken(ctx, &store.OAuthToken{Username: "leaver", Provider: store.Jira, AccessToken: "a", Expiry: now})
	_ = st.PutAgent(ctx, &store.Agent{ClientID: "ticket-bot", Owner: "leaver", Enabled: true})

	var out bytes.Buffer
	if err := runUserCmd(ctx, st, []string{"offboard", "-username", "leaver", "-operator", "secops"}, &out, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "revoked 1 key(s)") || !strings.Contains(out.String(), "ticket-bot") {
		t.Fatalf("output:\n%s", out.String())
	}
	if e := st.AuditEntries(); len(e) != 1 || e[0].Principal != "secops" {
		t.Fatalf("audit = %+v", e)
	}
	for _, args := range [][]string{{"offboard"}, {"delete", "-username", "x"}, {"offboard", "-bogus"}} {
		if err := runUserCmd(ctx, st, args, &out, now); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestHTTPClientTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)

	c, err := httpClient(&config.Config{HTTPTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Get(slow.URL); err == nil {
		t.Fatal("request to a hanging server did not time out")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}

	if _, err := httpClient(&config.Config{CAFile: "/nonexistent"}); err == nil {
		t.Fatal("missing CA_FILE accepted")
	}
}

type fakeSchema struct{ migrated, checked bool }

func (f *fakeSchema) Migrate(context.Context) error     { f.migrated = true; return nil }
func (f *fakeSchema) CheckSchema(context.Context) error { f.checked = true; return nil }

func TestPrepareSchema(t *testing.T) {
	f := &fakeSchema{}
	if err := prepareSchema(context.Background(), f, true); err != nil || !f.migrated || f.checked {
		t.Fatalf("migrate on start: %+v %v", f, err)
	}
	f = &fakeSchema{}
	if err := prepareSchema(context.Background(), f, false); err != nil || f.migrated || !f.checked {
		t.Fatalf("runtime role must only check: %+v %v", f, err)
	}
}

// TestListenTLS serves over TLS with the configured certificate and refuses plain HTTP.
func TestListenTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, pool := selfSigned(t, dir)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	srv := &http.Server{
		Addr:      addr,
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	done := make(chan error, 1)
	go func() { done <- listen(srv, certFile, keyFile) }()
	t.Cleanup(func() {
		_ = srv.Close()
		if err := <-done; err != nil {
			t.Errorf("listen: %v", err)
		}
	})

	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = client.Get("https://" + addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("https: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" || resp.TLS == nil {
		t.Fatalf("https response %q tls=%v", b, resp.TLS != nil)
	}

	plain, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr)
	if err == nil {
		defer plain.Body.Close()
		if plain.StatusCode != http.StatusBadRequest {
			t.Fatalf("plain HTTP accepted: %d", plain.StatusCode)
		}
	}

	old := &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11}
	if _, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: old}}).Get("https://" + addr); err == nil {
		t.Fatal("TLS 1.1 accepted")
	}
}

func selfSigned(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}
