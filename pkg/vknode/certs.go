package vknode

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
)

// certReloadInterval is how often the serving certificate is re-read. A
// kubelet-serving certificate is short-lived, so whatever renews it does so
// while the process is running; the process has to notice.
const certReloadInterval = time.Minute

// CertReloader serves the certificate currently on disk.
//
// Reading it once at startup means a renewed certificate is not used until the
// process restarts, so it expires while a perfectly good replacement sits in
// the same file — and the failure is that logs and exec stop working, with the
// node still Ready and nothing in its status suggesting a certificate.
type CertReloader struct {
	certPath, keyPath string

	mu      sync.RWMutex
	current *tls.Certificate
}

// NewCertReloader loads the pair and returns a reloader for it.
func NewCertReloader(certPath, keyPath string) (*CertReloader, error) {
	r := &CertReloader{certPath: certPath, keyPath: keyPath}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate is the tls.Config callback, called per handshake, so a
// reloaded certificate is used by the next connection without restarting the
// listener.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current, nil
}

func (r *CertReloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load serving certificate %s: %w", r.certPath, err)
	}
	r.mu.Lock()
	r.current = &cert
	r.mu.Unlock()
	return nil
}

// Run re-reads the certificate until the context is cancelled.
//
// A failed read leaves the last good certificate in place: a half-written file
// during renewal is a moment, and refusing every connection over it would be
// worse than serving the old certificate a minute longer.
func (r *CertReloader) Run(ctx context.Context) {
	t := time.NewTicker(certReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := r.expiry()
			if err := r.reload(); err != nil {
				log.G(ctx).WithError(err).
					Warn("could not re-read the serving certificate; keeping the one in use")
				continue
			}
			if after := r.expiry(); !after.Equal(before) {
				log.G(ctx).WithField("expires", after).
					Info("serving certificate reloaded")
			}
		}
	}
}

// expiry reports when the certificate in use stops being valid, which is the
// one thing worth logging about a reload.
func (r *CertReloader) expiry() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.current == nil || r.current.Leaf == nil {
		return time.Time{}
	}
	return r.current.Leaf.NotAfter
}
