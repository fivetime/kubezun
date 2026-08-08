package ingress

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/pools"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/fivetime/kubezun/pkg/service"
)

// backendRef identifies one Ingress backend (Service + port) — the unit that
// maps to one Octavia pool.
type backendRef struct {
	Service string
	Port    networkingv1.ServiceBackendPort
}

func (b backendRef) portKey() string {
	if b.Port.Name != "" {
		return b.Port.Name
	}
	return fmt.Sprintf("%d", b.Port.Number)
}

// collectBackends returns every distinct backend the Ingress references: the
// default backend (if any) plus each rule path's.
func collectBackends(ing *networkingv1.Ingress) (defaultBackend *backendRef, all []backendRef) {
	seen := map[string]bool{}
	add := func(b *networkingv1.IngressBackend) *backendRef {
		if b == nil || b.Service == nil {
			return nil
		}
		ref := backendRef{Service: b.Service.Name, Port: b.Service.Port}
		key := ref.Service + "|" + ref.portKey()
		if !seen[key] {
			seen[key] = true
			all = append(all, ref)
		}
		return &ref
	}
	defaultBackend = add(ing.Spec.DefaultBackend)
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for i := range rule.HTTP.Paths {
			add(&rule.HTTP.Paths[i].Backend)
		}
	}
	return defaultBackend, all
}

// poolName derives the stable pool name for one backend.
func poolName(lbname string, b backendRef) string {
	return fmt.Sprintf("%s_%s_%s", lbname, b.Service, b.portKey())
}

// resolveServicePort maps an Ingress backend port (name OR number) onto the
// backing Service's ServicePort; members then resolve the endpoint port
// through the EndpointSlice by that ServicePort's name.
func resolveServicePort(svc *corev1.Service, port networkingv1.ServiceBackendPort) (corev1.ServicePort, error) {
	for _, sp := range svc.Spec.Ports {
		if port.Name != "" && sp.Name == port.Name {
			return sp, nil
		}
		if port.Name == "" && sp.Port == port.Number {
			return sp, nil
		}
	}
	return corev1.ServicePort{}, fmt.Errorf("service %s has no port %q/%d", svc.Name, port.Name, port.Number)
}

// ensureBackendPool reconciles one backend into a pool + HTTP health monitor.
// Members are synced separately and deliberately after the listener: member
// churn flips the load balancer to PENDING_UPDATE, and a listener whose TLS
// ref needs repair must never be starved behind it.
func (r *Reconciler) ensureBackendPool(ctx context.Context, lb *loadbalancers.LoadBalancer, lbname string, b backendRef) (*pools.Pool, error) {
	name := poolName(lbname, b)
	pool, err := getPoolByName(ctx, r.Octavia, lb.ID, name)
	if err != nil && err != service.ErrNotFound {
		return nil, fmt.Errorf("looking up pool %q: %w", name, err)
	}
	if err == service.ErrNotFound {
		pool, err = service.CreatePool(ctx, r.Octavia, lb.ID, pools.CreateOpts{
			Name:           name,
			Protocol:       pools.ProtocolHTTP,
			LBMethod:       pools.LBMethodRoundRobin,
			LoadbalancerID: lb.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("creating pool %q: %w", name, err)
		}
	}

	// An HTTP health monitor is the L7 layer's real gain over the L4 tier:
	// "port open but the application answers 500" is invisible to a TCP
	// check and visible here.
	if pool.MonitorID == "" {
		if _, err := service.CreateHealthMonitor(ctx, r.Octavia, lb.ID, monitors.CreateOpts{
			Name:           name,
			PoolID:         pool.ID,
			Type:           "HTTP",
			URLPath:        "/",
			Delay:          5,
			Timeout:        3,
			MaxRetries:     2,
			MaxRetriesDown: 2,
		}); err != nil {
			return nil, fmt.Errorf("creating HTTP health monitor for pool %q: %w", name, err)
		}
	}
	return pool, nil
}

// syncPoolMembers puts the full desired member set of one backend pool: ready
// pods' capsule addresses + their subnets — pkg/service's seam reused, which
// is the single data-plane difference from every node-based ingress
// controller.
func (r *Reconciler) syncPoolMembers(ctx context.Context, lb *loadbalancers.LoadBalancer, ing *networkingv1.Ingress, b backendRef, poolID string) error {
	svc, err := r.Services.Services(ing.Namespace).Get(b.Service)
	if err != nil {
		return fmt.Errorf("reading backend Service %s: %w", b.Service, err)
	}
	sp, err := resolveServicePort(svc, b.Port)
	if err != nil {
		return err
	}
	sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: b.Service})
	found, err := r.Slices.EndpointSlices(ing.Namespace).List(sel)
	if err != nil {
		return err
	}
	slices := make([]discoveryv1.EndpointSlice, 0, len(found))
	for _, s := range found {
		slices = append(slices, *s)
	}
	family := corev1.IPv4Protocol
	if strings.Contains(lb.VipAddress, ":") {
		family = corev1.IPv6Protocol
	}
	members, err := service.BuildMembers(ctx, r.Subnets, slices, sp, family)
	if err != nil {
		return err
	}
	if err := service.SetPoolMembers(ctx, r.Octavia, lb.ID, poolID, members); err != nil {
		return fmt.Errorf("updating members of pool %s (%d members): %w", poolID, len(members), err)
	}
	return nil
}

// desiredPolicy is one host/path rule as an Octavia l7policy + its rules.
type desiredPolicy struct {
	Name     string
	PoolID   string
	Host     string
	Path     string
	PathType networkingv1.PathType
}

// signature flattens a policy (desired or existing) for drift comparison.
func (d desiredPolicy) signature() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", d.Name, d.PoolID, d.Host, d.Path, d.PathType)
}

// buildDesiredPolicies orders the Ingress rules into l7policies. Octavia
// evaluates policies by position; Kubernetes wants the most specific path
// first — longer paths sort ahead so "/api" wins over "/" no matter how the
// manifest ordered them.
func buildDesiredPolicies(ing *networkingv1.Ingress, lbname string, poolIDs map[string]string) []desiredPolicy {
	var out []desiredPolicy
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for i := range rule.HTTP.Paths {
			p := &rule.HTTP.Paths[i]
			if p.Backend.Service == nil {
				continue
			}
			ref := backendRef{Service: p.Backend.Service.Name, Port: p.Backend.Service.Port}
			poolID := poolIDs[poolName(lbname, ref)]
			if poolID == "" {
				continue // pool ensure failed upstream; skip rather than mis-route
			}
			pathType := networkingv1.PathTypePrefix
			if p.PathType != nil {
				pathType = *p.PathType
			}
			out = append(out, desiredPolicy{
				PoolID:   poolID,
				Host:     rule.Host,
				Path:     p.Path,
				PathType: pathType,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Path) > len(out[j].Path) })
	for i := range out {
		out[i].Name = fmt.Sprintf("%s_l7_%d", lbname, i)
	}
	return out
}

// syncL7Policies converges the listener's policies onto desired. Drift
// detection is whole-set: policies are ordered (position matters) and few, so
// on any difference the set is torn down and recreated in order — simpler and
// safer than in-place position surgery.
func syncL7Policies(ctx context.Context, octavia *gophercloud.ServiceClient, lbID, listenerID, lbname string, desired []desiredPolicy) error {
	existing, err := listL7Policies(ctx, octavia, listenerID)
	if err != nil {
		return fmt.Errorf("listing l7policies: %w", err)
	}
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].Position < existing[j].Position })

	match := len(existing) == len(desired)
	if match {
		for i := range existing {
			got, err := existingSignature(ctx, octavia, &existing[i])
			if err != nil {
				return err
			}
			if got != desired[i].signature() {
				match = false
				break
			}
		}
	}
	if match {
		return nil
	}

	for i := range existing {
		// Only our own policies are ours to delete; anything else on the
		// listener was made out-of-band and is left alone.
		if !strings.HasPrefix(existing[i].Name, lbname+"_l7_") {
			continue
		}
		if err := deleteL7Policy(ctx, octavia, lbID, existing[i].ID); err != nil {
			return fmt.Errorf("deleting stale l7policy %s: %w", existing[i].Name, err)
		}
	}
	for i := range desired {
		d := &desired[i]
		pol, err := createL7Policy(ctx, octavia, lbID, l7policies.CreateOpts{
			Name:           d.Name,
			ListenerID:     listenerID,
			Action:         l7policies.ActionRedirectToPool,
			RedirectPoolID: d.PoolID,
			Position:       int32(i + 1),
		})
		if err != nil {
			return fmt.Errorf("creating l7policy %s: %w", d.Name, err)
		}
		for _, rule := range rulesFor(*d) {
			if err := createL7Rule(ctx, octavia, lbID, pol.ID, rule); err != nil {
				return fmt.Errorf("creating l7rule for %s: %w", d.Name, err)
			}
		}
	}
	return nil
}

// rulesFor renders the host/path of one policy as Octavia l7rules — all rules
// of a policy AND together, exactly the Ingress "host AND path" semantics.
func rulesFor(d desiredPolicy) []l7policies.CreateRuleOpts {
	var rules []l7policies.CreateRuleOpts
	if d.Host != "" {
		rules = append(rules, l7policies.CreateRuleOpts{
			RuleType:    l7policies.TypeHostName,
			CompareType: l7policies.CompareTypeEqual,
			Value:       d.Host,
		})
	}
	if d.Path != "" && d.Path != "/" {
		compare := l7policies.CompareTypeStartWith
		if d.PathType == networkingv1.PathTypeExact {
			compare = l7policies.CompareTypeEqual
		}
		rules = append(rules, l7policies.CreateRuleOpts{
			RuleType:    l7policies.TypePath,
			CompareType: compare,
			Value:       d.Path,
		})
	}
	return rules
}

// existingSignature reconstructs a desiredPolicy-style signature from a live
// policy + its rules, so drift comparison sees through Octavia's data model.
func existingSignature(ctx context.Context, octavia *gophercloud.ServiceClient, pol *l7policies.L7Policy) (string, error) {
	rules, err := listL7Rules(ctx, octavia, pol.ID)
	if err != nil {
		return "", fmt.Errorf("listing rules of l7policy %s: %w", pol.Name, err)
	}
	got := desiredPolicy{Name: pol.Name, PoolID: pol.RedirectPoolID}
	for i := range rules {
		switch rules[i].RuleType {
		case string(l7policies.TypeHostName):
			got.Host = rules[i].Value
		case string(l7policies.TypePath):
			got.Path = rules[i].Value
			got.PathType = networkingv1.PathTypePrefix
			if rules[i].CompareType == string(l7policies.CompareTypeEqual) {
				got.PathType = networkingv1.PathTypeExact
			}
		}
	}
	// A desired "/"-Prefix path renders no path rule (matches everything), so
	// a live policy without one reads back as "/" Prefix.
	if got.Path == "" {
		got.Path, got.PathType = "/", networkingv1.PathTypePrefix
	}
	return got.signature(), nil
}

// gcStalePools removes pools of ours on the load balancer that no current
// backend references. After policy sync, so no policy still points at one;
// the listener default pool is skipped (re-pointed by ensureListener already).
func gcStalePools(ctx context.Context, octavia *gophercloud.ServiceClient, lbID, lbname string, keep map[string]bool, defaultPoolID string) error {
	all, err := listPools(ctx, octavia, lbID)
	if err != nil {
		return fmt.Errorf("listing pools: %w", err)
	}
	for i := range all {
		p := &all[i]
		if !strings.HasPrefix(p.Name, lbname+"_") || keep[p.ID] || p.ID == defaultPoolID {
			continue
		}
		if err := deletePool(ctx, octavia, lbID, p.ID); err != nil {
			return fmt.Errorf("deleting stale pool %s: %w", p.Name, err)
		}
	}
	return nil
}

// ensureListener reconciles THE listener of the Ingress load balancer:
// TERMINATED_HTTPS :443 with Barbican refs when spec.tls is set, plain HTTP
// :80 otherwise. Existing drift (rotated certificate, changed default pool,
// tuning annotations) is applied in place.
func ensureListener(ctx context.Context, octavia *gophercloud.ServiceClient, lbID, lbname string, tlsRefs []string, defaultPoolID string, tuning *listenerTuning) (*listeners.Listener, error) {
	proto, port, name := listeners.ProtocolHTTP, 80, fmt.Sprintf("%s_http_80", lbname)
	if len(tlsRefs) > 0 {
		proto, port, name = listeners.ProtocolTerminatedHTTPS, 443, fmt.Sprintf("%s_https_443", lbname)
	}

	listener, err := service.GetListenerByName(ctx, octavia, lbID, name)
	if err != nil && err != service.ErrNotFound {
		return nil, fmt.Errorf("looking up listener %q: %w", name, err)
	}
	if err == service.ErrNotFound {
		opts := listeners.CreateOpts{
			Name:           name,
			Protocol:       proto,
			ProtocolPort:   port,
			LoadbalancerID: lbID,
		}
		if len(tlsRefs) > 0 {
			opts.DefaultTlsContainerRef = tlsRefs[0]
			if len(tlsRefs) > 1 {
				opts.SniContainerRefs = tlsRefs[1:]
			}
		}
		if defaultPoolID != "" {
			opts.DefaultPoolID = defaultPoolID
		}
		if len(tuning.AllowedCIDRs) > 0 {
			opts.AllowedCIDRs = tuning.AllowedCIDRs
		}
		opts.TimeoutClientData = tuning.TimeoutClientData
		opts.TimeoutMemberData = tuning.TimeoutMemberData
		opts.TimeoutMemberConnect = tuning.TimeoutMemberConnect
		opts.TimeoutTCPInspect = tuning.TimeoutTCPInspect
		return service.CreateListener(ctx, octavia, lbID, opts)
	}

	var update listeners.UpdateOpts
	dirty := false
	if len(tlsRefs) > 0 && listener.DefaultTlsContainerRef != tlsRefs[0] {
		update.DefaultTlsContainerRef = &tlsRefs[0]
		dirty = true
	}
	if listener.DefaultPoolID != defaultPoolID {
		update.DefaultPoolID = &defaultPoolID
		dirty = true
	}
	// Whitelist: always enforced, including back to empty (see listenerTuning
	// for why a removed whitelist must not silently stay).
	if !cidrsEqual(listener.AllowedCIDRs, tuning.AllowedCIDRs) {
		cidrs := tuning.AllowedCIDRs
		update.AllowedCIDRs = &cidrs
		dirty = true
	}
	// Timeouts: enforced only while their annotation manages them.
	if timeoutDrift(tuning.TimeoutClientData, listener.TimeoutClientData) {
		update.TimeoutClientData = tuning.TimeoutClientData
		dirty = true
	}
	if timeoutDrift(tuning.TimeoutMemberData, listener.TimeoutMemberData) {
		update.TimeoutMemberData = tuning.TimeoutMemberData
		dirty = true
	}
	if timeoutDrift(tuning.TimeoutMemberConnect, listener.TimeoutMemberConnect) {
		update.TimeoutMemberConnect = tuning.TimeoutMemberConnect
		dirty = true
	}
	if timeoutDrift(tuning.TimeoutTCPInspect, listener.TimeoutTCPInspect) {
		update.TimeoutTCPInspect = tuning.TimeoutTCPInspect
		dirty = true
	}
	if dirty {
		if err := updateListener(ctx, octavia, lbID, listener.ID, update); err != nil {
			return nil, fmt.Errorf("updating listener %q: %w", name, err)
		}
		return service.GetListenerByName(ctx, octavia, lbID, name)
	}
	return listener, nil
}
