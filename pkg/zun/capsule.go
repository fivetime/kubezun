package zun

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
)

// Capsule is the subset of Zun's capsule representation this provider reads.
//
// gophercloud's own capsules.Capsule is not used for responses: it types
// restart_policy as a string while Zun answers with an object, so decoding a
// real capsule fails outright. Declaring only the fields that are actually
// consumed also keeps this decoding tolerant of the fields Zun adds over time.
type Capsule struct {
	UUID         string               `json:"uuid"`
	Status       string               `json:"status"`
	StatusReason string               `json:"status_reason"`
	Host         string               `json:"host"`
	CreatedAt    Time                 `json:"created_at"`
	Addresses    map[string][]Address `json:"addresses"`
	Containers   []Container          `json:"containers"`

	// Zun renames these two below microversion 1.32: the current API answers
	// with name/labels, older ones with meta_name/meta_labels. Both spellings
	// are decoded so the provider keeps working if the microversion is pinned
	// back, and Name/Labels resolve whichever arrived.
	NameField      string            `json:"name"`
	LabelsField    map[string]string `json:"labels"`
	MetaNameField  string            `json:"meta_name"`
	MetaLabelField map[string]string `json:"meta_labels"`
}

// Name is the capsule's name under either microversion.
func (c *Capsule) Name() string {
	if c.NameField != "" {
		return c.NameField
	}
	return c.MetaNameField
}

// Labels are the capsule's labels under either microversion.
func (c *Capsule) Labels() map[string]string {
	if len(c.LabelsField) > 0 {
		return c.LabelsField
	}
	return c.MetaLabelField
}

// Address is one of a capsule's Neutron addresses.
type Address struct {
	Addr             string  `json:"addr"`
	Port             string  `json:"port"`
	Version          float64 `json:"version"`
	SubnetID         string  `json:"subnet_id"`
	PreserveOnDelete bool    `json:"preserve_on_delete"`
}

// Container is one container inside a capsule.
type Container struct {
	UUID         string `json:"uuid"`
	Name         string `json:"name"`
	Image        string `json:"image"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	StatusDetail string `json:"status_detail"`
	StartedAt    Time   `json:"started_at"`
	UpdatedAt    Time   `json:"updated_at"`

	// Healthcheck carries both the probe definitions and the state the prober
	// keeps for them; readiness lives under k8s_probe_state.
	Healthcheck map[string]any `json:"healthcheck"`
}

// Ready reports whether the container is serving, as its readiness probe last
// answered. A container with no readiness probe is ready as soon as it runs,
// which is what Kubernetes does: absent a probe there is nothing that could
// say otherwise.
func (c *Container) Ready() bool {
	state, ok := c.Healthcheck["k8s_probe_state"].(map[string]any)
	if !ok {
		return true
	}
	ready, ok := state["ready"].(bool)
	if !ok {
		return true
	}
	return ready
}

// HasReadinessProbe reports whether a readiness probe was declared for this
// container. Until the prober has run once there is no state to read, and a
// container must not be called ready on the strength of a probe that has not
// answered yet.
func (c *Container) HasReadinessProbe() bool {
	probes, ok := c.Healthcheck["k8s_probes"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = probes["readinessProbe"]
	return ok
}

// ProbeAnswered reports whether the prober has recorded a readiness result.
func (c *Container) ProbeAnswered() bool {
	state, ok := c.Healthcheck["k8s_probe_state"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = state["ready"]
	return ok
}

// Time decodes the timestamps Zun emits. They are neither RFC 3339 nor
// consistently formatted — "2026-08-06 19:02:52" alongside ISO forms — so a
// plain time.Time field fails to decode an ordinary capsule.
type Time struct{ time.Time }

var zunTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05.999999",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
}

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	for _, layout := range zunTimeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	// An unparseable timestamp is not worth failing a status sync over: the
	// zero value only costs a start time in the pod's status.
	t.Time = time.Time{}
	return nil
}

// CapsuleAPI wraps the capsule calls this provider makes.
type CapsuleAPI struct {
	client *Client
}

// NewCapsuleAPI returns an API bound to a tenant client.
func NewCapsuleAPI(c *Client) *CapsuleAPI { return &CapsuleAPI{client: c} }

func (a *CapsuleAPI) url(parts ...string) string {
	return a.client.ServiceClient().ServiceURL(append([]string{"capsules"}, parts...)...)
}

// Create submits a capsule template. Zun answers 202 and builds the capsule
// asynchronously, so the returned capsule carries little beyond its identity.
func (a *CapsuleAPI) Create(ctx context.Context, tpl []byte) (*Capsule, error) {
	var out Capsule
	_, err := a.client.ServiceClient().Post(ctx, a.url(), map[string]any{
		"template": string(tpl),
	}, &out, &gophercloud.RequestOpts{OkCodes: []int{200, 201, 202}})
	if err != nil {
		return nil, translate(err)
	}
	return &out, nil
}

// Get fetches one capsule by name or UUID.
func (a *CapsuleAPI) Get(ctx context.Context, id string) (*Capsule, error) {
	var out Capsule
	_, err := a.client.ServiceClient().Get(ctx, a.url(id), &out,
		&gophercloud.RequestOpts{OkCodes: []int{200, 203}})
	if err != nil {
		return nil, translate(err)
	}
	return &out, nil
}

// Delete removes a capsule. force is required because Zun answers 409 for a
// capsule that is not already stopped, which is every capsule backing a
// running pod: without it the pod would hang in Terminating forever.
func (a *CapsuleAPI) Delete(ctx context.Context, id string) error {
	_, err := a.client.ServiceClient().Delete(ctx, a.url(id)+"?force=True",
		&gophercloud.RequestOpts{OkCodes: []int{200, 202, 204}})
	return translate(err)
}

// ListManagedAll returns every capsule this provider owns, grouped by pod.
//
// A pod can map to more than one capsule: Zun does not enforce unique names,
// so a retried create leaves duplicates behind, and each duplicate holds
// resources of its own. Callers that only need current state can use
// ListManaged; cleanup needs to see them all.
func (a *CapsuleAPI) ListManagedAll(ctx context.Context) (map[string][]*Capsule, error) {
	var body struct {
		Capsules []Capsule `json:"capsules"`
	}
	_, err := a.client.ServiceClient().Get(ctx, a.url(), &body,
		&gophercloud.RequestOpts{OkCodes: []int{200, 203}})
	if err != nil {
		return nil, translate(err)
	}

	out := make(map[string][]*Capsule, len(body.Capsules))
	for i := range body.Capsules {
		c := &body.Capsules[i]
		ns, name, ok := PodKeyFromLabels(c.Labels())
		if !ok {
			// A capsule the tenant created through the Zun API directly is not
			// this node's to touch.
			continue
		}
		key := PodKey(ns, name)
		out[key] = append(out[key], c)
	}
	return out, nil
}

// ListManaged returns one capsule per pod: the one whose state the pod's
// status should reflect. Where duplicates exist the newest wins, since that is
// the one the most recent create produced.
func (a *CapsuleAPI) ListManaged(ctx context.Context) (map[string]*Capsule, error) {
	all, err := a.ListManagedAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Capsule, len(all))
	for key, capsules := range all {
		newest := capsules[0]
		for _, c := range capsules[1:] {
			if c.CreatedAt.After(newest.CreatedAt.Time) {
				newest = c
			}
		}
		out[key] = newest
	}
	return out, nil
}

// PodUID reports the pod a capsule was created for. Recreating a pod under the
// same name yields a different UID, which is what distinguishes a capsule left
// behind by the old pod from the one backing the new one.
func (c *Capsule) PodUID() string { return c.Labels()[LabelPodUID] }

// NodeName returns the virtual node that created the capsule, or "" for a
// capsule created before nodes stamped their name on one.
func (c *Capsule) NodeName() string { return c.Labels()[LabelNodeName] }

// translate maps OpenStack transport errors onto the kinds the node controller
// understands; without this a capsule that is already gone reads as a hard
// failure and its pod is retried forever.
func translate(err error) error {
	if err == nil {
		return nil
	}
	var unexpected gophercloud.ErrUnexpectedResponseCode
	if asUnexpected(err, &unexpected) {
		switch unexpected.Actual {
		case http.StatusNotFound:
			return errdefs.AsNotFound(err)
		case http.StatusConflict, http.StatusBadRequest:
			return errdefs.AsInvalidInput(err)
		}
	}
	return err
}

func asUnexpected(err error, target *gophercloud.ErrUnexpectedResponseCode) bool {
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
