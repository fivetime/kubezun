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
		// curl first for its richer HTTP handling, with -k because in-pod TLS
		// is typically self-signed; wget is the busybox and alpine default.
		// An image with neither fails loudly rather than reporting healthy.
		cmd := fmt.Sprintf(
			`if command -v curl >/dev/null 2>&1; then exec curl -fsk -o /dev/null -m %d %q; `+
				`elif command -v wget >/dev/null 2>&1; then exec wget -q -O /dev/null -T %d %q; `+
				`else echo "kubezun probe: image has no curl or wget" >&2; exit 1; fi`,
			timeout, url, timeout, url)
		out.HTTPGet = nil
		out.Exec = &corev1.ExecAction{Command: []string{"sh", "-c", cmd}}

	case p.TCPSocket != nil:
		port, err := resolveProbePort(p.TCPSocket.Port, c)
		if err != nil {
			return nil, err
		}
		// curl's telnet scheme is the fallback: exit code 7 is specifically
		// "failed to connect", so anything else means the port answered.
		cmd := fmt.Sprintf(
			`if command -v nc >/dev/null 2>&1; then exec nc -z -w %d 127.0.0.1 %d; `+
				`elif command -v curl >/dev/null 2>&1; then curl -s -m %d telnet://127.0.0.1:%d </dev/null >/dev/null 2>&1; [ $? -ne 7 ]; `+
				`else echo "kubezun probe: image has no nc or curl" >&2; exit 1; fi`,
			timeout, port, timeout, port)
		out.TCPSocket = nil
		out.Exec = &corev1.ExecAction{Command: []string{"sh", "-c", cmd}}

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
