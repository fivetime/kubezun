// Package volume turns a tenant's PersistentVolumeClaims into OpenStack
// storage, on the tenant's own credentials.
//
// There is no CSI driver here and there deliberately will not be one: a
// cluster-wide CSI controller holds one cloud credential for every tenant it
// serves, which is the arrangement this platform exists to avoid. This process
// already holds one tenant's application credential and already reconciles
// Services and Ingresses with it; storage is the same shape.
//
// What the tenant sees is unchanged: a PersistentVolumeClaim binds to a
// PersistentVolume and a pod mounts it. Where the storage came from is not
// their problem.
package volume

import (
	"context"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/sharedfilesystems/v2/shareaccessrules"
	"github.com/gophercloud/gophercloud/v2/openstack/sharedfilesystems/v2/shares"
)

// Kind is which OpenStack service backs a claim. The claim's StorageClass
// decides it: a catalog entry's provisioner names the service, and
// ReadWriteMany on a block-device class is refused rather than served by
// something that shares a device instead of a filesystem.
type Kind string

const (
	Block  Kind = "cinder"
	Shared Kind = "manila"
)

// Drivers are the names put on the PersistentVolume. They are shaped like CSI
// driver names so that a PV written today still reads correctly if a real CSI
// driver ever takes this over.
const (
	BlockDriver  = "cinder.knaas.io"
	SharedDriver = "manila.knaas.io"
)

// Backend provisions and removes the storage behind a claim.
type Backend struct {
	Block  *gophercloud.ServiceClient
	Shared *gophercloud.ServiceClient

	// ShareType is the Manila share type shares are created with; empty lets
	// Manila choose its default.
	ShareType string
	// ShareProto is the protocol shares are exported with. NFS is what a
	// capsule's node can mount without extra client software.
	ShareProto string
	// VolumeType is the Cinder volume type; empty lets Cinder choose.
	VolumeType string
	// AvailabilityZone places the storage where the capsules are.
	AvailabilityZone string
}

// Provisioned is what a claim was given.
type Provisioned struct {
	Kind Kind
	// ID is the Cinder volume id or the Manila share id.
	ID string
	// Export is where a shared filesystem is reached, as
	// "host:/path". Empty for a block device.
	Export string
	// GiB is the size that was actually created, which Manila and Cinder both
	// round up to whole gibibytes.
	GiB int
}

// Create makes the storage for one claim. storageType overrides the
// deployment default -- it is what a catalog class's parameters chose, the
// Cinder volume type or the Manila share type, which is the whole reason the
// catalog exists: the tier is the tenant's choice, priced accordingly, not a
// deployment constant hidden behind an opaque class name.
func (b *Backend) Create(ctx context.Context, name string, kind Kind, gib int, description, storageType string) (*Provisioned, error) {
	if gib < 1 {
		// Both services count in gibibytes and neither accepts zero. A claim
		// for less than one gets one rather than an error, which is what
		// rounding up a size means.
		gib = 1
	}
	switch kind {
	case Block:
		if b.Block == nil {
			return nil, fmt.Errorf("this deployment has no block storage endpoint")
		}
		vtype := b.VolumeType
		if storageType != "" {
			vtype = storageType
		}
		v, err := volumes.Create(ctx, b.Block, volumes.CreateOpts{
			Name:             name,
			Size:             gib,
			VolumeType:       vtype,
			AvailabilityZone: b.AvailabilityZone,
			Description:      description,
		}, nil).Extract()
		if err != nil {
			return nil, fmt.Errorf("creating a %dGiB volume: %w", gib, err)
		}
		return &Provisioned{Kind: Block, ID: v.ID, GiB: v.Size}, nil

	case Shared:
		if b.Shared == nil {
			return nil, fmt.Errorf("this deployment has no shared filesystem endpoint; ReadWriteMany needs one")
		}
		proto := b.ShareProto
		if proto == "" {
			proto = "NFS"
		}
		stype := b.ShareType
		if storageType != "" {
			stype = storageType
		}
		s, err := shares.Create(ctx, b.Shared, shares.CreateOpts{
			Name:             name,
			Size:             gib,
			ShareProto:       proto,
			ShareType:        stype,
			AvailabilityZone: b.AvailabilityZone,
			Description:      description,
		}).Extract()
		if err != nil {
			return nil, fmt.Errorf("creating a %dGiB share: %w", gib, err)
		}
		return &Provisioned{Kind: Shared, ID: s.ID, GiB: s.Size}, nil
	}
	return nil, fmt.Errorf("unknown storage kind %q", kind)
}

// ExportOf returns where a share is mounted from, once Manila has decided.
//
// A share has no export location until it is available, so this is asked again
// rather than waited on inside a reconcile: a claim whose share is still being
// built stays Pending, which is what Pending is for.
func (b *Backend) ExportOf(ctx context.Context, shareID string) (string, error) {
	list, err := shares.ListExportLocations(ctx, b.Shared, shareID).Extract()
	if err != nil {
		return "", err
	}
	// Prefer the location Manila marks preferred; it is the one it expects
	// clients to use when a backend offers several.
	for _, l := range list {
		if l.IsAdminOnly {
			continue
		}
		if l.Preferred {
			return l.Path, nil
		}
	}
	for _, l := range list {
		if !l.IsAdminOnly {
			return l.Path, nil
		}
	}
	return "", nil
}

// Grant lets a client address mount a share.
//
// Manila's NFS backends authorise by address, so the address that has to be
// granted is the node the capsule runs on -- the mount happens there, and the
// container reaches it through the mount. That is also the uncomfortable part
// of this design and it is not hidden: a node granted access to a share can
// reach it on behalf of anything running there, so the grant must go no wider
// than the nodes of the tenant that owns the share.
func (b *Backend) Grant(ctx context.Context, shareID, client string) error {
	_, err := shares.GrantAccess(ctx, b.Shared, shareID, shares.GrantAccessOpts{
		AccessType:  "ip",
		AccessTo:    client,
		AccessLevel: "rw",
	}).Extract()
	if err != nil && !isAlreadyGranted(err) {
		return fmt.Errorf("granting %s access to share %s: %w", client, shareID, err)
	}
	return nil
}

// Revoke withdraws a grant made by Grant.
func (b *Backend) Revoke(ctx context.Context, shareID, client string) error {
	rules, err := shareaccessrules.List(ctx, b.Shared, shareID).Extract()
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.AccessTo != client {
			continue
		}
		if err := shares.RevokeAccess(ctx, b.Shared, shareID,
			shares.RevokeAccessOpts{AccessID: r.ID}).ExtractErr(); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the storage behind a claim.
func (b *Backend) Delete(ctx context.Context, kind Kind, id string) error {
	switch kind {
	case Block:
		err := volumes.Delete(ctx, b.Block, id, volumes.DeleteOpts{}).ExtractErr()
		if err != nil && !gophercloud.ResponseCodeIs(err, 404) {
			return err
		}
	case Shared:
		err := shares.Delete(ctx, b.Shared, id).ExtractErr()
		if err != nil && !gophercloud.ResponseCodeIs(err, 404) {
			return err
		}
	}
	return nil
}

func isAlreadyGranted(err error) bool {
	return gophercloud.ResponseCodeIs(err, 400) &&
		strings.Contains(strings.ToLower(err.Error()), "exist")
}
