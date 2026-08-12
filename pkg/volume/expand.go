package volume

import (
	"context"
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/sharedfilesystems/v2/shares"
)

// extendSettle bounds how long an extend is waited on. Cinder answers the call
// and does the work afterwards, so the size that matters is the one read back,
// not the one that was asked for.
const extendSettle = 2 * time.Minute

// Expand grows the storage behind a claim to at least gib, and returns the size
// it actually has afterwards.
//
// ⚠️ Reads the size back rather than assuming the call worked. An expansion
// that quietly did less than was asked for is discovered when the filesystem
// fills, which is both later and harder to connect to this. The pattern is
// nova's (nova/virt/incus/driver.py:12808-12828), which refuses rather than
// believes the number it is handed.
func (b *Backend) Expand(ctx context.Context, kind Kind, id string, gib int) (int, error) {
	switch kind {
	case Block:
		if b.Block == nil {
			return 0, fmt.Errorf("this deployment has no block storage endpoint")
		}
		current, err := volumes.Get(ctx, b.Block, id).Extract()
		if err != nil {
			return 0, fmt.Errorf("reading volume %s: %w", id, err)
		}
		if current.Size >= gib {
			// Already big enough: an expansion asked for twice, or one that
			// finished while nobody was looking. Saying so beats a second
			// extend, which Cinder refuses.
			return current.Size, nil
		}
		// ⚠️ Extending a volume that is in use needs microversion 3.42; below
		// it Cinder answers "status must be available to extend", which reads
		// as a rule about the volume rather than about the request. Asked for
		// on a copy of the client, because raising it on the shared one would
		// change every other call this process makes. Same shape as
		// cloud-provider-openstack pkg/csi/cinder/openstack/openstack_volumes.go:383-405.
		client := b.Block
		if current.Status == "in-use" {
			copied := *b.Block
			copied.Microversion = "3.42"
			client = &copied
		}
		if err := volumes.ExtendSize(ctx, client, id,
			volumes.ExtendSizeOpts{NewSize: gib}).ExtractErr(); err != nil {
			return current.Size, fmt.Errorf("extending volume %s to %dGiB: %w", id, gib, err)
		}
		return b.awaitBlockSize(ctx, id, gib)

	case Shared:
		if b.Shared == nil {
			return 0, fmt.Errorf("this deployment has no shared filesystem endpoint")
		}
		current, err := shares.Get(ctx, b.Shared, id).Extract()
		if err != nil {
			return 0, fmt.Errorf("reading share %s: %w", id, err)
		}
		if current.Size >= gib {
			return current.Size, nil
		}
		if err := shares.Extend(ctx, b.Shared, id,
			shares.ExtendOpts{NewSize: gib}).ExtractErr(); err != nil {
			return current.Size, fmt.Errorf("extending share %s to %dGiB: %w", id, gib, err)
		}
		return b.awaitShareSize(ctx, id, gib)
	}
	return 0, fmt.Errorf("unknown storage kind %q", kind)
}

// awaitBlockSize waits for Cinder to finish, and says what it settled on.
//
// ⚠️ "extending" is a state the volume passes through, and reading the size
// during it gives the old one. Reporting that upward would tell Kubernetes the
// claim is still small when it is about not to be, and the resize would look
// like it failed every time it succeeded.
func (b *Backend) awaitBlockSize(ctx context.Context, id string, want int) (int, error) {
	deadline := time.Now().Add(extendSettle)
	last := 0
	for time.Now().Before(deadline) {
		v, err := volumes.Get(ctx, b.Block, id).Extract()
		if err != nil {
			return last, err
		}
		last = v.Size
		switch v.Status {
		case "extending":
			// Still working.
		case "error_extending":
			return last, fmt.Errorf(
				"volume %s could not be extended to %dGiB; it is %s at %dGiB",
				id, want, v.Status, v.Size)
		default:
			if v.Size >= want {
				return v.Size, nil
			}
			// Settled at a size smaller than asked for. Not an outcome to
			// report as success.
			return v.Size, fmt.Errorf(
				"volume %s settled at %dGiB, short of the %dGiB asked for",
				id, v.Size, want)
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return last, err
		}
	}
	return last, fmt.Errorf("volume %s was still extending after %s", id, extendSettle)
}

func (b *Backend) awaitShareSize(ctx context.Context, id string, want int) (int, error) {
	deadline := time.Now().Add(extendSettle)
	last := 0
	for time.Now().Before(deadline) {
		s, err := shares.Get(ctx, b.Shared, id).Extract()
		if err != nil {
			return last, err
		}
		last = s.Size
		switch s.Status {
		case "extending":
		case "extending_error":
			return last, fmt.Errorf(
				"share %s could not be extended to %dGiB; it is %s at %dGiB",
				id, want, s.Status, s.Size)
		default:
			if s.Size >= want {
				return s.Size, nil
			}
			return s.Size, fmt.Errorf(
				"share %s settled at %dGiB, short of the %dGiB asked for",
				id, s.Size, want)
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return last, err
		}
	}
	return last, fmt.Errorf("share %s was still extending after %s", id, extendSettle)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
