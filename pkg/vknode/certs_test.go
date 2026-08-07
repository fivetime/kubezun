package vknode

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePair(t *testing.T, dir, cn string, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// A renewed certificate has to be picked up while the process runs. Read once
// at startup, it expires with its replacement sitting unread in the same file,
// and what fails is logs and exec, on a node that still reports Ready.
func TestCertReloaderPicksUpARenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writePair(t, dir, "first", time.Now().Add(time.Hour))

	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	first, _ := r.GetCertificate(nil)
	if first.Leaf == nil || first.Leaf.Subject.CommonName != "first" {
		t.Fatalf("loaded the wrong certificate: %+v", first.Leaf)
	}

	writePair(t, dir, "renewed", time.Now().Add(48*time.Hour))
	if err := r.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, _ := r.GetCertificate(nil)
	if got.Leaf.Subject.CommonName != "renewed" {
		t.Errorf("still serving %q after renewal", got.Leaf.Subject.CommonName)
	}
}

// Renewal is not atomic: a half-written file is a moment, and refusing every
// connection over it would be worse than serving the previous certificate for
// a minute longer.
func TestCertReloaderKeepsTheLastGoodOne(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writePair(t, dir, "good", time.Now().Add(time.Hour))

	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	if err := os.WriteFile(certPath, []byte("half written"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(); err == nil {
		t.Error("an unreadable certificate was accepted")
	}
	got, _ := r.GetCertificate(nil)
	if got == nil || got.Leaf.Subject.CommonName != "good" {
		t.Error("the working certificate was dropped when the file went bad")
	}
}
