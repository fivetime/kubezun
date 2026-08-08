// Command probe performs one httpGet or tcpSocket check and exits 0 if it
// passed.
//
// It exists because a capsule's probe has to run inside the container. Nothing
// outside can reach it: the address is on the tenant's OVN network, and neither
// the compute node's root namespace nor the sandbox's own network namespace can
// reach it — measured on all three lab compute nodes, where the sandbox
// namespace holds only the tap and the address itself lives inside the Kata VM.
//
// So the check runs in the container, and the container is often distroless.
// Rewriting a probe into `sh -c curl` fails on registry.k8s.io images, on
// FROM scratch images, and on most Go applications — CoreDNS among them, which
// is the tenant's own resolver. Those images have no shell to report the
// problem either, so the failure reads as the application being unhealthy.
//
// This binary is mounted read-only into the container from the compute node
// rather than built into the image or shipped inside each capsule, so it costs
// nothing per capsule and needs nothing of the tenant.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	var (
		url     = flag.String("http", "", "URL to GET; passes on a 2xx or 3xx response")
		addr    = flag.String("tcp", "", "host:port to connect to; passes if the connection is accepted")
		timeout = flag.Duration("timeout", time.Second, "how long the check may take")
	)
	flag.Parse()

	var err error
	switch {
	case *url != "":
		err = httpProbe(*url, *timeout)
	case *addr != "":
		err = tcpProbe(*addr, *timeout)
	default:
		fmt.Fprintln(os.Stderr, "probe: one of -http or -tcp is required")
		os.Exit(2)
	}

	if err != nil {
		// The message is the whole of what the tenant will see about a failing
		// probe, since the container writes nothing about it, so it says what
		// was attempted rather than only what went wrong.
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func httpProbe(url string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// In-pod TLS is typically self-signed, and a probe is checking
			// that the application answers rather than who it is: it is
			// already talking to 127.0.0.1 inside the container.
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
		// Kubernetes counts a redirect as a pass, so following one would turn
		// a 302 to a broken location into a failure it does not report.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	// The same range kubelet treats as healthy.
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return nil
}

func tcpProbe(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	return conn.Close()
}
