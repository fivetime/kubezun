package zun

import (
	"context"
	"fmt"
	"net/url"

	"github.com/gophercloud/gophercloud/v2"
)

// ExtendVolume makes a capsule see a volume that has already been grown.
//
// ⚠️ Two halves, and this is only the second. The volume service grew the
// volume; what remains can only be done where it is attached -- the kernel has
// to re-read the device and the filesystem on it has to be grown. Until that
// happens the pod sees the old size however large the volume is, which is
// indistinguishable from an expansion that failed.
//
// A shared filesystem needs none of this: the file server owns the filesystem,
// so a share is simply larger.
func (a *CapsuleAPI) ExtendVolume(ctx context.Context, podUID, volumeID string, gib int) error {
	if podUID == "" || volumeID == "" {
		return fmt.Errorf("a pod UID and a volume id are required to extend")
	}
	q := url.Values{}
	q.Set("volume_id", volumeID)
	q.Set("size", fmt.Sprintf("%d", gib))
	name := "kubezun-" + podUID
	_, err := a.sc.Post(ctx,
		a.url(name, "extend_volume")+"?"+q.Encode(), nil, nil,
		&gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}})
	if err != nil {
		return translate(err)
	}
	return nil
}
