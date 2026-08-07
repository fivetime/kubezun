// Command virtual-kubelet registers one tenant's KNaaS virtual node and runs
// its pods as OpenStack Zun capsules.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	vklog "github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"

	knode "github.com/fivetime/kubezun/pkg/node"
	"github.com/fivetime/kubezun/pkg/provider"
	"github.com/fivetime/kubezun/pkg/zun"
)

// version reported as the node's kubelet version.
var version = "v1.36.3-knaas.1"

type options struct {
	nodeName    string
	tenant      string
	namespaces  string
	zone        string
	zunAZ       string
	networkID   string
	kubeconfig  string
	listenAddr  string
	internalIP  string
	capacityCPU string
	capacityMem string
	capacityPod string
	logLevel    string
}

func main() {
	var o options
	flag.StringVar(&o.nodeName, "nodename", os.Getenv("KUBEZUN_NODE_NAME"),
		"name to register this virtual node under")
	flag.StringVar(&o.tenant, "tenant", os.Getenv("KUBEZUN_TENANT"),
		"tenant id; becomes the node's pool label and taint value")
	flag.StringVar(&o.namespaces, "namespaces", os.Getenv("KUBEZUN_NAMESPACES"),
		"comma-separated namespaces this node serves; pods from any other namespace are refused")
	flag.StringVar(&o.zone, "zone", os.Getenv("KUBEZUN_ZONE"),
		"topology zone, mapped onto the Zun availability zone")
	flag.StringVar(&o.zunAZ, "zun-availability-zone", os.Getenv("KUBEZUN_ZUN_AZ"),
		"Zun availability zone capsules are placed in; defaults to letting Zun choose. "+
			"This is not the same namespace of names as --zone, which is the Kubernetes "+
			"topology label, so the two are configured separately")
	flag.StringVar(&o.networkID, "network-id", os.Getenv("KUBEZUN_NETWORK_ID"),
		"tenant Neutron network capsules attach to")
	flag.StringVar(&o.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig")
	flag.StringVar(&o.listenAddr, "listen", envOr("KUBEZUN_LISTEN", ":10250"),
		"address the kubelet API listens on")
	flag.StringVar(&o.internalIP, "internal-ip", os.Getenv("KUBEZUN_INTERNAL_IP"),
		"address the API server reaches this process on; required for logs and exec")
	flag.StringVar(&o.capacityCPU, "capacity-cpu", envOr("KUBEZUN_CAPACITY_CPU", "0"),
		"node CPU capacity; mirror the tenant's quota")
	flag.StringVar(&o.capacityMem, "capacity-memory", envOr("KUBEZUN_CAPACITY_MEMORY", "0"),
		"node memory capacity; mirror the tenant's quota")
	flag.StringVar(&o.capacityPod, "capacity-pods", envOr("KUBEZUN_CAPACITY_PODS", "0"),
		"maximum pods; mirror the tenant's quota")
	flag.StringVar(&o.logLevel, "log-level", envOr("KUBEZUN_LOG_LEVEL", "info"), "log level")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if o.nodeName == "" {
		return fmt.Errorf("--nodename is required")
	}
	namespaces := splitAndTrim(o.namespaces)
	if len(namespaces) == 0 {
		// Serving every namespace would make this node reachable by any pod
		// whose spec.nodeName names it, which is exactly the boundary the
		// namespace list exists to enforce.
		return fmt.Errorf("--namespaces is required: a node must state which namespaces it serves")
	}

	logger := logrus.StandardLogger()
	lvl, err := logrus.ParseLevel(o.logLevel)
	if err != nil {
		return fmt.Errorf("parse log level: %w", err)
	}
	logger.SetLevel(lvl)
	vklog.L = logruslogger.FromLogrus(logrus.NewEntry(logger))

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	creds, err := zun.CredentialsFromEnv()
	if err != nil {
		return fmt.Errorf("read OpenStack credentials: %w", err)
	}
	zunClient, err := zun.NewClient(ctx, creds)
	if err != nil {
		return err
	}

	nodeSpec := knode.Build(knode.Options{
		Name:       o.nodeName,
		Tenant:     o.tenant,
		Zone:       o.zone,
		Version:    version,
		InternalIP: o.internalIP,
		Capacity: knode.Capacity{
			CPU:    o.capacityCPU,
			Memory: o.capacityMem,
			Pods:   o.capacityPod,
		},
	})

	newProvider := func(cfg nodeutil.ProviderConfig) (nodeutil.Provider, vknode.NodeProvider, error) {
		p, err := provider.New(provider.Config{
			Namespaces:       namespaces,
			NetworkID:        o.networkID,
			AvailabilityZone: o.zunAZ,
			NodeName:         o.nodeName,
		}, zunClient)
		if err != nil {
			return nil, nil, err
		}
		// Carry the node spec built above onto the object nodeutil registers.
		cfg.Node.Labels = nodeSpec.Labels
		cfg.Node.Spec.Taints = nodeSpec.Spec.Taints
		cfg.Node.Status.NodeInfo = nodeSpec.Status.NodeInfo
		cfg.Node.Status.Capacity = nodeSpec.Status.Capacity
		cfg.Node.Status.Allocatable = nodeSpec.Status.Allocatable
		cfg.Node.Status.Addresses = nodeSpec.Status.Addresses
		cfg.Node.Status.DaemonEndpoints = nodeSpec.Status.DaemonEndpoints
		cfg.Node.Status.Conditions = []corev1.NodeCondition{knode.ReadyCondition()}
		return p, nil, nil
	}

	n, err := nodeutil.NewNode(o.nodeName, newProvider,
		func(c *nodeutil.NodeConfig) error {
			c.KubeconfigPath = o.kubeconfig
			c.HTTPListenAddr = o.listenAddr
			return nil
		},
	)
	if err != nil {
		return err
	}

	go func() {
		if err := n.Run(ctx); err != nil {
			vklog.G(ctx).WithError(err).Error("node stopped")
			cancel()
		}
	}()

	if err := n.WaitReady(ctx, 0); err != nil {
		return fmt.Errorf("wait for node to become ready: %w", err)
	}
	vklog.G(ctx).WithField("node", o.nodeName).
		WithField("namespaces", namespaces).Info("virtual node is ready")

	<-n.Done()
	return n.Err()
}

func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
