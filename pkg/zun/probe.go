package zun

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// A probe against a capsule can only run inside the container.
//
// Nothing outside it can reach the address the probe names: the compute host
// is not on the tenant's OVN network, and a kata sandbox's network namespace
// holds only a tap device — the network stack lives inside the microVM
// (measured; see DESIGN §6.0). Running the same check inside the container
// against 127.0.0.1 preserves the meaning, because the application is
// listening in exactly that namespace either way.
//
// This is the pattern Istio uses for rewriteAppHTTPProbers and the one
// kubetron arrived at for the same reason on its own path. Only the handler is
// swapped: initialDelaySeconds, periodSeconds, failureThreshold and the rest
// describe when to probe, not how, and are left untouched.
//
// The rewrite applies to the capsule template alone. The pod in Kubernetes
// keeps the probe its author wrote, so `kubectl get pod -o yaml` shows what was
// asked for rather than the shell this turns it into.

// RewriteProbe converts a network probe into an exec probe against the
// container itself. Exec probes are returned unchanged: they need no network.
// ProbeHelper is where the compute node's probe helper appears inside a
// container. It must match Zun's probe_helper_mount.
//
// A network probe becomes an exec because nothing outside the container can
// reach it: the address is on the tenant's OVN network and, under Kata, lives
// inside the VM rather than in the sandbox's namespace on the host — measured
// on every lab compute node, from the root namespace and from each network
// namespace on it.
//
// It runs this helper rather than the image's own tools. Shelling out to curl
// or wget works on a distribution image and fails on everything distroless:
// registry.k8s.io images, FROM scratch images, most Go applications — CoreDNS
// among them, which is a tenant's own resolver. Such an image has no shell to
// report the problem either, so a container answering perfectly well reads as
// unhealthy, and the tenant is told their application is failing.
const ProbeHelper = "/.kubezun/probe"

func RewriteProbe(p *corev1.Probe, c *corev1.Container) (*corev1.Probe, error) {
	if p == nil {
		return nil, nil
	}
	if p.Exec != nil {
		return p, nil
	}

	out := p.DeepCopy()
	timeout := probeTimeout(p)

	switch {
	case p.HTTPGet != nil:
		port, err := resolveProbePort(p.HTTPGet.Port, c)
		if err != nil {
			return nil, err
		}
		scheme := "http"
		if p.HTTPGet.Scheme == corev1.URISchemeHTTPS {
			scheme = "https"
		}
		path := p.HTTPGet.Path
		if path == "" {
			path = "/"
		}
		url := fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, port, path)
		out.HTTPGet = nil
		out.Exec = &corev1.ExecAction{Command: []string{
			ProbeHelper, "-http", url, "-timeout", fmt.Sprintf("%ds", timeout),
		}}

	case p.TCPSocket != nil:
		port, err := resolveProbePort(p.TCPSocket.Port, c)
		if err != nil {
			return nil, err
		}
		out.TCPSocket = nil
		out.Exec = &corev1.ExecAction{Command: []string{
			ProbeHelper, "-tcp", fmt.Sprintf("127.0.0.1:%d", port),
			"-timeout", fmt.Sprintf("%ds", timeout),
		}}

	case p.GRPC != nil:
		// grpc_health_probe predates the native probe and most gRPC images
		// still ship it, which gives real serving-status semantics. A TCP
		// connect is the degraded fallback: an open port is not SERVING, but
		// it beats a probe that could never succeed.
		svc := ""
		if p.GRPC.Service != nil && *p.GRPC.Service != "" {
			svc = fmt.Sprintf(" -service=%q", *p.GRPC.Service)
		}
		cmd := fmt.Sprintf(
			`if command -v grpc_health_probe >/dev/null 2>&1; then exec grpc_health_probe -addr=127.0.0.1:%d%s -connect-timeout=%ds -rpc-timeout=%ds; `+
				`elif command -v grpc-health-probe >/dev/null 2>&1; then exec grpc-health-probe -addr=127.0.0.1:%d%s -connect-timeout=%ds -rpc-timeout=%ds; `+
				`elif command -v nc >/dev/null 2>&1; then exec nc -z -w %d 127.0.0.1 %d; `+
				`else echo "kubezun probe: image has no grpc_health_probe or nc" >&2; exit 1; fi`,
			p.GRPC.Port, svc, timeout, timeout,
			p.GRPC.Port, svc, timeout, timeout,
			timeout, p.GRPC.Port)
		out.GRPC = nil
		out.Exec = &corev1.ExecAction{Command: []string{"sh", "-c", cmd}}

	default:
		return nil, unsupported("probe", "no handler is set on the probe")
	}

	return out, nil
}

// resolveProbePort turns a numeric or named port into a number using the
// container's own declarations, which is the resolution the kubelet performs.
func resolveProbePort(port intstr.IntOrString, c *corev1.Container) (int32, error) {
	if port.Type == intstr.Int {
		return port.IntVal, nil
	}
	for _, cp := range c.Ports {
		if cp.Name == port.StrVal {
			return cp.ContainerPort, nil
		}
	}
	return 0, unsupported("probe port",
		fmt.Sprintf("named port %q is not declared in the container's ports",
			port.StrVal))
}

// probeTimeout returns the probe's timeout, defaulting as Kubernetes does.
func probeTimeout(p *corev1.Probe) int32 {
	if p.TimeoutSeconds > 0 {
		return p.TimeoutSeconds
	}
	return 1
}
