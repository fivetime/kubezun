// Command virtual-kubelet registers one tenant's KNaaS virtual node and runs
// its pods as OpenStack Zun capsules.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	vklog "github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"

	knode "github.com/fivetime/kubezun/pkg/node"
	"github.com/fivetime/kubezun/pkg/provider"
	kservice "github.com/fivetime/kubezun/pkg/service"
	vkset "github.com/fivetime/kubezun/pkg/vknode"
	"github.com/fivetime/kubezun/pkg/zun"
)

// version reported as the node's kubelet version.
var version = "v1.36.3-knaas.1"

type options struct {
	nodeName      string
	tenant        string
	namespaces    string
	zone          string
	zunAZ         string
	arch          string
	networkID     string
	kubeconfig    string
	listenAddr    string
	internalIP    string
	capacityCPU   string
	capacityMem   string
	capacityPod   string
	logLevel      string
	nodes         nodeSpecList
	tlsCert       string
	tlsKey        string
	clientCA      string
	vipSubnet     string
	vipNetwork    string
	floatingNet   string
	clusterDomain string
	publicSvcs    bool
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
	flag.StringVar(&o.arch, "arch", envOr("KUBEZUN_ARCH", "amd64"),
		"machine architecture this node's capsules run on (amd64, arm64, ppc64le, "+
			"s390x, riscv64). Becomes the node's kubernetes.io/arch label and a "+
			"required Placement trait, so a pod scheduled here lands on a host that "+
			"can actually execute its image. Run one node per architecture.")
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
	flag.StringVar(&o.vipNetwork, "vip-network-id", os.Getenv("KUBEZUN_VIP_NETWORK_ID"),
		"network the VIP subnet belongs to; the Service address port is created on it")
	flag.StringVar(&o.vipSubnet, "vip-subnet-id", os.Getenv("KUBEZUN_VIP_SUBNET_ID"),
		"subnet a Service's load balancer address comes from. Not the pod subnet: "+
			"an address there makes east-west traffic arrive with the wrong "+
			"destination MAC. Without it, Services get no load balancer")
	flag.StringVar(&o.clusterDomain, "cluster-domain", envOr("KUBEZUN_CLUSTER_DOMAIN", "svc.cluster.local"),
		"suffix a Service's name resolves under. A Service's usable address is its "+
			"load balancer's, not the ClusterIP, so the name is how a pod reaches "+
			"one. Empty disables naming")
	flag.StringVar(&o.floatingNet, "floating-network-id", os.Getenv("KUBEZUN_FLOATING_NETWORK_ID"),
		"external network public Service addresses are allocated from")
	flag.BoolVar(&o.publicSvcs, "public-services-by-default", false,
		"give a LoadBalancer Service a public address when it does not say. Off: a "+
			"public address is billed, and a chart copied from elsewhere saying "+
			"type: LoadBalancer has not asked to spend one. A Service overrides "+
			"this either way with "+kservice.InternalAnnotation)
	flag.StringVar(&o.tlsCert, "tls-cert-file", os.Getenv("KUBEZUN_TLS_CERT_FILE"),
		"certificate the kubelet API serves. Its SANs must cover --internal-ip, which "+
			"is the address the API server dials; node names never appear in the "+
			"certificate, so one covers every node in this process")
	flag.StringVar(&o.tlsKey, "tls-private-key-file", os.Getenv("KUBEZUN_TLS_KEY_FILE"),
		"private key for --tls-cert-file")
	flag.StringVar(&o.clientCA, "client-ca-file", os.Getenv("KUBEZUN_CLIENT_CA_FILE"),
		"CA that signs the API server's client certificate. Without it the kubelet "+
			"API cannot tell the API server from any other caller")
	flag.Var(&o.nodes, "node",
		"an extra virtual node to run in this process, as "+
			"name=<n>[,arch=<a>][,zone=<z>][,zun-az=<az>][,listen=<addr>]; repeatable. "+
			"Anything unstated is taken from the flags above. A tenant needs one node "+
			"per architecture and per availability zone, and running them here rather "+
			"than one process each shares the pod watch and the credentials between them.")
	flag.StringVar(&o.logLevel, "log-level", envOr("KUBEZUN_LOG_LEVEL", "info"), "log level")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	specs, err := nodeSpecs(o)
	if err != nil {
		return err
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

	client, err := nodeutil.ClientsetFromEnv(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("build Kubernetes client: %w", err)
	}

	set, err := vkset.NewSet(vkset.SetOptions{
		Client:     client,
		Namespaces: namespaces,
		Workers:    runtime.NumCPU(),
	})
	if err != nil {
		return err
	}

	tlsConfig, err := serverTLS(o)
	if err != nil {
		return err
	}

	for _, spec := range specs {
		nodeObj := knode.Build(knode.Options{
			Name:       spec.name,
			Tenant:     o.tenant,
			Zone:       spec.zone,
			Arch:       spec.arch,
			Version:    version,
			InternalIP: o.internalIP,
			Capacity: knode.Capacity{
				CPU:    o.capacityCPU,
				Memory: o.capacityMem,
				Pods:   o.capacityPod,
			},
		})
		nodeObj.Status.Conditions = []corev1.NodeCondition{knode.ReadyCondition()}

		p, err := provider.New(provider.Config{
			Namespaces:       namespaces,
			NetworkID:        o.networkID,
			AvailabilityZone: spec.zunAZ,
			Architecture:     spec.arch,
			NodeName:         spec.name,
			ClusterDomain:    o.clusterDomain,
		}, zunClient, provider.Caches{
			Pods:       set.Pods(),
			ConfigMaps: set.ConfigMaps(),
			Secrets:    set.Secrets(),
		})
		if err != nil {
			return err
		}

		var auth nodeutil.Auth
		if tlsConfig != nil {
			// The authorizer names this node, so the API server's permission to
			// read one node's logs does not extend to another's. That is why
			// each node needs its own listen address rather than sharing one.
			auth, err = nodeutil.WebhookAuth(client, spec.name, withClientCA(o.clientCA))
			if err != nil {
				return fmt.Errorf("node %s: kubelet API auth: %w", spec.name, err)
			}
		}

		if _, err := set.AddNode(vkset.NodeOptions{
			Spec:     nodeObj,
			Provider: p,
			Auth:     auth,
			// A node provider of our own, so the node's Ready condition tracks
			// whether Zun can be reached: with the default one the node stays
			// Ready no matter what and the scheduler keeps sending pods to a
			// node that cannot create a capsule.
			NodeProvider: provider.NewNodeHealth(zunClient, nodeObj),
			ListenAddr:   spec.listen,
			TLSConfig:    tlsConfig,
		}); err != nil {
			return err
		}
	}

	if o.vipSubnet != "" {
		// One controller per process, not per node: a load balancer belongs to
		// the tenant, and every one of their virtual nodes serves the same
		// Services.
		octavia, err := kservice.NewOctaviaClient(zunClient)
		if err != nil {
			return fmt.Errorf("build the load balancer client: %w", err)
		}
		neutron, err := kservice.NewNetworkClient(zunClient)
		if err != nil {
			return fmt.Errorf("build the network client: %w", err)
		}
		controller, err := kservice.NewController(&kservice.Reconciler{
			Octavia:           octavia,
			Neutron:           neutron,
			Subnets:           kservice.NewCapsuleSubnets(zun.NewCapsuleAPI(zunClient)),
			Services:          set.ServiceInformer().Lister(),
			Slices:            set.EndpointSliceInformer().Lister(),
			ServiceClient:     client.CoreV1(),
			VIPSubnetID:       o.vipSubnet,
			VIPNetworkID:      o.vipNetwork,
			FloatingNetworkID: o.floatingNet,
			PublicByDefault:   o.publicSvcs,
			ClusterDomain:     o.clusterDomain,
			Tenant:            o.tenant,
		}, set.ServiceInformer(), set.EndpointSliceInformer())
		if err != nil {
			return err
		}
		go controller.Run(ctx)
		// Load balancers of Services deleted while this was not running are
		// invisible to the queue; nothing but a sweep ever removes them.
		go controller.RunGC(ctx)
	} else {
		vklog.G(ctx).Warn("no --vip-subnet-id; Services get no load balancer, " +
			"and a pod cannot reach a Service by its cluster address")
	}

	go func() {
		if err := set.Run(ctx); err != nil {
			vklog.G(ctx).WithError(err).Error("node set stopped")
			cancel()
		}
	}()

	if err := set.WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for nodes to become ready: %w", err)
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.name+"("+spec.arch+")")
	}
	vklog.G(ctx).WithField("nodes", strings.Join(names, " ")).
		WithField("namespaces", namespaces).Info("virtual nodes are ready")

	<-ctx.Done()
	return nil
}

// serverTLS builds the kubelet API's TLS config, or nil when no certificate was
// given.
//
// The certificate covers an address, not a node: the API server dials whatever
// is in the node's status, and node names never appear in TLS. So one
// certificate serves every node in this process, and the per-node part is the
// authorizer, not the key material.
func serverTLS(o options) (*tls.Config, error) {
	if o.tlsCert == "" && o.tlsKey == "" {
		return nil, nil
	}
	if o.tlsCert == "" || o.tlsKey == "" {
		return nil, fmt.Errorf("--tls-cert-file and --tls-private-key-file must be given together")
	}
	if o.clientCA == "" {
		// Without it the server cannot tell the API server from anyone else who
		// can reach the port, and logs and exec reach into tenant containers.
		return nil, fmt.Errorf(
			"--client-ca-file is required with a serving certificate: without it the " +
				"kubelet API cannot verify who is calling")
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		CipherSuites: nodeutil.DefaultServerCiphers(),
	}
	if err := nodeutil.WithKeyPairFromPath(o.tlsCert, o.tlsKey)(cfg); err != nil {
		return nil, fmt.Errorf("load serving certificate: %w", err)
	}
	if err := nodeutil.WithCAFromPath(o.clientCA)(cfg); err != nil {
		return nil, fmt.Errorf("load client CA: %w", err)
	}
	return cfg, nil
}

// withClientCA makes the kubelet API verify the caller's certificate against
// the given CA.
//
// Without a CA provider the delegating authenticator does not do mTLS at all,
// so the API server would be indistinguishable from any other caller that
// reached the port — and every route behind it reads or enters a tenant's
// containers.
func withClientCA(path string) nodeutil.WebhookAuthOption {
	return func(cfg *nodeutil.WebhookAuthConfig) error {
		ca, err := dynamiccertificates.NewDynamicCAContentFromFile("client-ca", path)
		if err != nil {
			return fmt.Errorf("load client CA %s: %w", path, err)
		}
		cfg.AuthnConfig.ClientCertificateCAContentProvider = ca
		return nil
	}
}

// nodeSpecs resolves the nodes this process runs. The single-node flags remain
// the defaults for every --node, and on their own they describe one node, which
// is what most tenants run.
func nodeSpecs(o options) ([]nodeSpec, error) {
	defaults := nodeSpec{
		arch:   o.arch,
		zone:   o.zone,
		zunAZ:  o.zunAZ,
		listen: o.listenAddr,
	}

	var specs []nodeSpec
	if o.nodeName != "" {
		defaults.name = o.nodeName
		specs = append(specs, defaults)
	}
	for _, raw := range o.nodes {
		spec, err := parseNodeSpec(raw, defaults)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no nodes: give --nodename or at least one --node")
	}

	seenName := map[string]bool{}
	seenAddr := map[string]bool{}
	for _, spec := range specs {
		if err := zun.ValidateArchitecture(spec.arch); err != nil {
			return nil, fmt.Errorf("node %s: %w", spec.name, err)
		}
		if seenName[spec.name] {
			// Two controllers on one node object would fight over its status
			// and each treat the other's pods as its own.
			return nil, fmt.Errorf("node %s is named twice", spec.name)
		}
		seenName[spec.name] = true
		if spec.listen != "" && seenAddr[spec.listen] {
			// Only one can bind it, and which one wins is a race; the loser
			// serves no kubelet API and its logs and exec fail with nothing
			// saying why.
			return nil, fmt.Errorf(
				"node %s listens on %s, which another node already uses; "+
					"give each node its own listen address", spec.name, spec.listen)
		}
		seenAddr[spec.listen] = true
	}
	return specs, nil
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
