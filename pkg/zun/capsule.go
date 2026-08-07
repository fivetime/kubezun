package zun

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/container/v1/capsules"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
)

// CapsuleAPI wraps the capsule calls this provider makes.
type CapsuleAPI struct {
	client *Client
}

// NewCapsuleAPI returns an API bound to a tenant client.
func NewCapsuleAPI(c *Client) *CapsuleAPI { return &CapsuleAPI{client: c} }

// Create submits a capsule template.
func (a *CapsuleAPI) Create(ctx context.Context, tpl []byte) (*capsules.Capsule, error) {
	opts := capsules.CreateOpts{TemplateOpts: &capsules.Template{Bin: tpl}}
	r := capsules.Create(ctx, a.client.ServiceClient(), opts)
	if r.Err != nil {
		return nil, translate(r.Err)
	}
	c, err := r.ExtractBase()
	if err != nil {
		// A 202 with a body this client cannot parse still means the capsule
		// was accepted; the status poll will pick it up.
		return nil, nil
	}
	return c, nil
}

// Get fetches one capsule by name or UUID.
func (a *CapsuleAPI) Get(ctx context.Context, id string) (*capsules.Capsule, error) {
	c, err := capsules.Get(ctx, a.client.ServiceClient(), id).ExtractBase()
	if err != nil {
		return nil, translate(err)
	}
	return c, nil
}

// Delete removes a capsule.
func (a *CapsuleAPI) Delete(ctx context.Context, id string) error {
	return translate(capsules.Delete(ctx, a.client.ServiceClient(), id).ExtractErr())
}

// ListManaged returns every capsule this provider owns, keyed by pod. Capsules
// the tenant created directly through the Zun API carry no ownership label and
// are skipped: deleting them as "orphans" would destroy work this node was
// never asked to manage.
func (a *CapsuleAPI) ListManaged(ctx context.Context) (map[string]*capsules.Capsule, error) {
	out := map[string]*capsules.Capsule{}
	err := capsules.List(a.client.ServiceClient(), nil).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := capsules.ExtractCapsulesBase(page)
			if err != nil {
				return false, err
			}
			for i := range list {
				c := &list[i]
				ns, name, ok := PodKeyFromLabels(c.MetaLabels)
				if !ok {
					continue
				}
				out[PodKey(ns, name)] = c
			}
			return true, nil
		})
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// translate maps OpenStack transport errors onto the error kinds the node
// controller understands; without this a deleted capsule reads as a hard
// failure and the pod is retried forever.
func translate(err error) error {
	if err == nil {
		return nil
	}
	var unexpected gophercloud.ErrUnexpectedResponseCode
	if errorsAs(err, &unexpected) {
		switch unexpected.Actual {
		case http.StatusNotFound:
			return errdefs.AsNotFound(err)
		case http.StatusConflict, http.StatusBadRequest:
			return errdefs.AsInvalidInput(err)
		}
	}
	return err
}

func errorsAs(err error, target *gophercloud.ErrUnexpectedResponseCode) bool {
	for err != nil {
		if e, ok := err.(gophercloud.ErrUnexpectedResponseCode); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// MarshalTemplate is a helper for callers holding a template value.
func MarshalTemplate(v any) ([]byte, error) { return json.Marshal(v) }
