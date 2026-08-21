// Command virtual-kubelet registers one tenant's KNaaS virtual node and runs
// its pods as OpenStack Zun capsules.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	vklog "github.com/virtual-kubelet/virtual-kubelet/log"
	logruslogger "github.com/virtual-kubelet/virtual-kubelet/log/logrus"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"

	kingress "github.com/fivetime/kubezun/pkg/ingress"
	"github.com/fivetime/kubezun/pkg/netpol"
	knode "github.com/fivetime/kubezun/pkg/node"
	"github.com/fivetime/kubezun/pkg/provider"
	kservice "github.com/fivetime/kubezun/pkg/service"
	"github.com/fivetime/kubezun/pkg/tenant"
	vkset "github.com/fivetime/kubezun/pkg/vknode"
	kvolume "github.com/fivetime/kubezun/pkg/volume"
	"github.com/fivetime/kubezun/pkg/zun"
)

// version reported as the node's kubelet version.
var version = "v1.36.3-knaas.1"

type options struct {
	nodeName             string
	tenant               string
	namespaces           string
	zone                 string
	zunAZ                string
	arch                 string
	networkID            string
	kubeconfig           string
	listenAddr           string
	internalIP           string
	capacityCPU          string
	capacityMem          string
	capacityPod          string
	capacityEphemeral    string
	logLevel             string
	nsSelector           string
	ingressProvider      string
	ingressClass         string
	leaseSeconds         int
	nodes                nodeSpecList
	tlsCert              string
	tlsKey               string
	clientCA             string
	vipSubnet            string
	vipNetwork           string
	floatingNet          string
	clusterDomain        string
	clusterDNS           string
	dnsService           string
	storageClass         string
	shareType            string
	volumeType           string
	storageAZ            string
	platformNamespace    string
	shard                string
	tenantLabel          string
	enforceNetworkPolicy bool
	convertNetworkPolicy string
	convertConfirm       bool
	publicSvcs           bool
}

func main() {
	var o options
	flag.StringVar(&o.nodeName, "nodename", os.Getenv("KUBEZUN_NODE_NAME"),
		"name to register this virtual node under")
	flag.StringVar(&o.tenant, "tenant", os.Getenv("KUBEZUN_TENANT"),
		"tenant id; becomes the node's pool label and taint value")
	flag.StringVar(&o.namespaces, "namespaces", os.Getenv("KUBEZUN_NAMESPACES"),
		"comma-separated namespaces this node serves. Superseded by "+
			"--namespace-selector, which tracks the namespaces a tenant creates "+
			"while this is running; a fixed list leaves a namespace made later "+
			"with no compute at all and nothing saying why")
	flag.StringVar(&o.nsSelector, "namespace-selector", os.Getenv("KUBEZUN_NAMESPACE_SELECTOR"),
		"label selector naming the namespaces this process serves, normally "+
			"kubezoo.io/tenant=<id>. The gateway writes that label on every "+
			"namespace it makes for a tenant and refuses a write that changes "+
			"it, so it is as hard to forge as the namespace name and, unlike a "+
			"name prefix, can be given to the API server as a selector")
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
	flag.StringVar(&o.capacityEphemeral, "capacity-ephemeral-storage",
		envOr("KUBEZUN_CAPACITY_EPHEMERAL", "200Gi"),
		"advertised ephemeral-storage capacity; without one, any pod declaring "+
			"the resource is unschedulable on this node")
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
	flag.StringVar(&o.clusterDNS, "cluster-dns", os.Getenv("KUBEZUN_CLUSTER_DNS"),
		"resolver addresses capsules are given, comma separated, as kubelet's "+
			"--cluster-dns. Normally left empty: the address is a load balancer "+
			"this process builds and cannot be known in advance, so it is read "+
			"from --dns-service instead")
	flag.StringVar(&o.storageClass, "storage-class", envOr("KUBEZUN_STORAGE_CLASS", "on"),
		"set empty to turn claim provisioning off. Which claims are served is "+
			"no longer named here: a claim is served when its StorageClass's "+
			"provisioner is cinder.knaas.io or manila.knaas.io, and the class's "+
			"parameters choose the volume or share type")
	flag.StringVar(&o.shareType, "share-type", os.Getenv("KUBEZUN_SHARE_TYPE"),
		"Manila share type ReadWriteMany claims are created with; empty lets "+
			"Manila pick its default")
	flag.StringVar(&o.volumeType, "volume-type", os.Getenv("KUBEZUN_VOLUME_TYPE"),
		"Cinder volume type claims are created with; empty lets Cinder pick "+
			"its default, which must map to a running backend")
	flag.StringVar(&o.convertNetworkPolicy, "convert-network-policy", "",
		"convert this tenant onto policy-decided security groups and exit: "+
			"'attach' adds them to every capsule port, 'detach' then removes the "+
			"project default. ⚠️ Run attach everywhere before detach anywhere: "+
			"pods reach each other by sharing the default group, so a tenant "+
			"half-way through detach is one whose halves cannot talk. Reports "+
			"what it would do unless --convert-confirm is also given")
	flag.BoolVar(&o.convertConfirm, "convert-confirm", false,
		"actually write the conversion, rather than reporting what it would change")
	flag.StringVar(&o.platformNamespace, "platform-namespace", os.Getenv("KUBEZUN_PLATFORM_NAMESPACE"),
		"namespace holding per-tenant credential Secrets. Setting it turns on "+
			"multi-tenant resolution: each namespace's work runs on its own "+
			"tenant's credential, found in <platform-namespace>/<tenant>. "+
			"⚠️ Never a tenant-visible namespace: a Secret does not have to be "+
			"visible to be used, and a pod can mount any Secret of its own "+
			"namespace (DESIGN §4.6.2)")
	flag.StringVar(&o.shard, "shard", os.Getenv("KUBEZUN_SHARD"),
		"shard name of this process under the shared-node form (DESIGN §3.4). "+
			"Set it with --platform-namespace and WITHOUT --tenant: nodes then "+
			"carry the knaas.io/shard label and the knaas.io/serverless taint "+
			"instead of any tenant identity")
	flag.StringVar(&o.tenantLabel, "tenant-label", envOr("KUBEZUN_TENANT_LABEL", "kubezoo.io/tenant"),
		"namespace label whose value names the tenant; the gateway writes it "+
			"and refuses changes, so it is as hard to forge as the namespace name")
	flag.BoolVar(&o.enforceNetworkPolicy, "enforce-network-policy",
		os.Getenv("KUBEZUN_ENFORCE_NETWORK_POLICY") == "true",
		"enforce NetworkPolicy by placing each capsule's port in security groups. "+
			"⚠️ Off by default and not a per-pod switch: a tenant's pods reach each "+
			"other because all of them are in the project's default group, whose "+
			"only ingress rule admits that same group, so a pod moved out of it "+
			"stops being accepted by every pod still in it. Turning this on "+
			"converts the whole tenant; see DESIGN §7.7.5a for the order")
	flag.StringVar(&o.storageAZ, "storage-availability-zone", os.Getenv("KUBEZUN_STORAGE_AZ"),
		"availability zone storage is created in; empty lets the service choose")
	flag.StringVar(&o.dnsService, "dns-service", envOr("KUBEZUN_DNS_SERVICE", "kube-system/kube-dns"),
		"Service whose address capsules resolve through, named as the tenant "+
			"writes it. Empty leaves capsules with the subnet's resolver, which "+
			"knows no in-cluster name")
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
	flag.IntVar(&o.leaseSeconds, "lease-duration", 40,
		"seconds a node's lease is valid, and so how often it is renewed. The "+
			"largest single lever on what a virtual node costs the control "+
			"plane; a longer one than a real kubelet's is defensible because a "+
			"virtual node's health is a process being up rather than a machine "+
			"still answering, and the cost is how long the scheduler keeps "+
			"placing pods on one whose process has died")
	flag.StringVar(&o.ingressProvider, "ingress-provider", os.Getenv("KUBEZUN_INGRESS_PROVIDER"),
		"Octavia provider for Ingress load balancers (amphora, or incus where its "+
			"driver serves L7). Empty disables Ingress entirely. Never \"ovn\": that "+
			"provider is L4-only and refuses every L7 object. Separate from the "+
			"Service provider on purpose — an L7 load balancer is real instances "+
			"with real cost, where a Service is OVN flows and free, which is why "+
			"turning this on is an operator decision rather than a default")
	flag.StringVar(&o.ingressClass, "ingress-class", envOr("KUBEZUN_INGRESS_CLASS", "knaas"),
		"ingress class this process answers for; anything else belongs to other "+
			"controllers")
	flag.StringVar(&o.logLevel, "log-level", envOr("KUBEZUN_LOG_LEVEL", "info"), "log level")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	// The conversion is an OpenStack operation and nothing else. Taking it
	// before the node and Kubernetes setup means an operator running it by
	// hand needs credentials and nothing more -- not a kubeconfig, not a node
	// name for nodes it will not register.
	if o.convertNetworkPolicy != "" {
		ctx, cancel := signal.NotifyContext(context.Background(),
			syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		creds, err := tenant.CredentialsFromEnv()
		if err != nil {
			return fmt.Errorf("read OpenStack credentials: %w", err)
		}
		session, err := tenant.NewSession(ctx, creds)
		if err != nil {
			return err
		}
		return convertTenant(ctx, session, netpol.Phase(o.convertNetworkPolicy),
			o.convertConfirm)
	}

	specs, err := nodeSpecs(o)
	if err != nil {
		return err
	}
	namespaces := splitAndTrim(o.namespaces)
	if len(namespaces) == 0 && o.nsSelector == "" {
		// Serving every namespace would make this node reachable by any pod
		// whose spec.nodeName names it, which is exactly the boundary this
		// exists to enforce.
		return fmt.Errorf(
			"one of --namespace-selector or --namespaces is required: " +
				"a node must state which namespaces it serves")
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

	creds, err := tenant.CredentialsFromEnv()
	if err != nil {
		return fmt.Errorf("read OpenStack credentials: %w", err)
	}
	session, err := tenant.NewSession(ctx, creds)
	if err != nil {
		return err
	}
	// One endpoint per process, shared by everything that talks to Zun: the
	// capsule API, the node's health check and the Service reconciler's subnet
	// lookup all resolve the same catalog entry.
	capsuleAPI, err := zun.NewCapsuleAPI(session)
	if err != nil {
		return err
	}

	client, err := nodeutil.ClientsetFromEnv(o.kubeconfig)
	if err != nil {
		return fmt.Errorf("build Kubernetes client: %w", err)
	}

	var watcher *vkset.Namespaces
	if o.nsSelector != "" {
		watcher = vkset.NewNamespacesWithTenantLabel(client, o.nsSelector, o.tenantLabel)
	}

	// Multi-tenant resolution. Nil in the single-tenant deployment, and every
	// seam below treats nil as "the fixed clients serve everything" — which is
	// what keeps this rollout safe to do in steps.
	var mt *multiTenant
	if o.platformNamespace != "" {
		// Shared-node capacity: static and large (DESIGN §3.2). The old
		// "mirror the tenant's quota" cannot mean anything on a node several
		// tenants share, and the flags' zero default would advertise a node
		// nothing can schedule onto. Real admission control is ResourceQuota
		// at admission and Zun's project quota — both real, neither here.
		if o.capacityCPU == "0" {
			o.capacityCPU = "1000"
		}
		if o.capacityMem == "0" {
			o.capacityMem = "4Ti"
		}
		if o.capacityPod == "0" {
			o.capacityPod = "10000"
		}
		if watcher == nil {
			return fmt.Errorf("--platform-namespace needs --namespace-selector: " +
				"per-tenant credentials are resolved through the namespaces' tenant label")
		}
		mt = newMultiTenant(client, watcher, o.platformNamespace)
		// Scoped caches (DESIGN §2.2): Services and EndpointSlices are watched
		// per served namespace instead of cluster-wide. Wired to the watcher
		// BEFORE it starts, so the initial namespaces arrive as Track calls.
		mt.scoped = vkset.NewScopedFactories(client, 0)
		watcher.OnChange(mt.scoped.Track)
		mt.scoped.Start(ctx)
	}

	set, err := vkset.NewSet(vkset.SetOptions{
		Client:               client,
		Namespaces:           namespaces,
		NamespaceWatcher:     watcher,
		Workers:              runtime.NumCPU(),
		LeaseDurationSeconds: o.leaseSeconds,
	})
	if err != nil {
		return err
	}

	tlsConfig, certs, err := serverTLS(o)
	if err != nil {
		return err
	}
	if certs != nil {
		go certs.Run(ctx)
	}

	// Ask Zun once, before any node registers, whether the zones these nodes
	// claim are real. A node whose zone does not exist still registers and
	// still accepts pods; each one is then refused by Zun's scheduler as "no
	// valid host", which reads like a full cluster rather than a typo in a
	// flag. Refusing to start says which zone and which ones exist.
	for _, spec := range specs {
		if err := zun.CheckAvailabilityZone(ctx, session, spec.zunAZ); err != nil {
			return fmt.Errorf("node %q: %w", spec.name, err)
		}
	}

	// Storage: claims become Cinder volumes or Manila shares on the tenant's
	// own credential. Before the node loop because each node's provider
	// resolves claims through it.
	var volRec *kvolume.Reconciler
	if o.storageClass != "" {
		blockC, err := kvolume.NewBlockStorageClient(session)
		if err != nil {
			vklog.G(ctx).WithError(err).Warn(
				"no block storage endpoint; ReadWriteOnce claims will be refused")
			blockC = nil
		}
		sharedC, err := kvolume.NewSharedFSClient(session)
		if err != nil {
			vklog.G(ctx).WithError(err).Warn(
				"no shared filesystem endpoint; ReadWriteMany claims will be refused")
			sharedC = nil
		}
		volRec = &kvolume.Reconciler{
			Backend: &kvolume.Backend{
				Block:            blockC,
				Shared:           sharedC,
				ShareType:        o.shareType,
				VolumeType:       o.volumeType,
				AvailabilityZone: o.storageAZ,
			},
			// ⚠️ Cluster-scoped kinds only; the namespaced listers are chosen
			// below, because merely CALLING set.ClaimInformer() materializes
			// the cluster-wide informer and the factory would start it.
			Volumes:     set.VolumeInformer().Lister(),
			Classes:     set.ClassInformer().Lister(),
			Client:      client.CoreV1(),
			PlacementOf: placementOf(specs, session.Region()),
			// Growing a block volume needs a second step where it is attached,
			// which only the capsule service can do.
			Capsules:        capsuleAPI,
			Tenant:          o.tenant,
			ServesNamespace: set.Serves,
		}
		var volCtl *kvolume.Controller
		if mt != nil {
			volRec.Claims = mt.scoped.ClaimLister()
			volRec.Pods = mt.scoped.PodLister()
			volCtl, err = kvolume.NewControllerFromSource(volRec, mt.scoped, set.VolumeInformer())
		} else {
			volRec.Claims = set.ClaimInformer().Lister()
			volRec.Pods = set.AllPodsInformer().Lister()
			volCtl, err = kvolume.NewController(volRec, set.ClaimInformer(), set.VolumeInformer())
		}
		if err != nil {
			return err
		}
		if mt != nil {
			mt.volume = volRec
			volCtl.ReconcilerFor = mt.volumeFor
			volCtl.EachReconciler = func(ctx context.Context, fn func(*kvolume.Reconciler) error) error {
				return eachTenant(ctx, mt, mt.volumeFor, fn)
			}
		}
		go volCtl.Run(ctx)
		go volCtl.RunGC(ctx)
	}

	// NetworkPolicy. Off unless asked for, because switching it on is a
	// tenant-wide conversion rather than a setting: pods reach each other by
	// sharing the project's default security group, so a half-converted tenant
	// is one where the converted and unconverted halves cannot talk, and the
	// failure lands on whichever side is still in the old group.
	var netpolRec *netpol.Reconciler
	if o.enforceNetworkPolicy {
		netC, err := netpol.NewClient(session)
		if err != nil {
			return fmt.Errorf("no network endpoint, which NetworkPolicy needs: %w", err)
		}
		netpolRec = &netpol.Reconciler{
			Neutron:         &netpol.Neutron{Client: netC},
			ServesNamespace: set.Serves,
		}
		var netpolCtl *netpol.Controller
		if mt != nil {
			netpolRec.Pods = mt.scoped.PodLister()
			netpolRec.Policies = mt.scoped.NetworkPolicyLister()
			netpolCtl, err = netpol.NewControllerFromSource(netpolRec, mt.scoped)
		} else {
			netpolRec.Pods = set.AllPodsInformer().Lister()
			netpolRec.Policies = set.PolicyInformer().Lister()
			netpolCtl, err = netpol.NewController(netpolRec,
				set.AllPodsInformer(), set.PolicyInformer())
		}
		if err != nil {
			return err
		}
		if mt != nil {
			mt.netpol = netpolRec
			netpolCtl.ReconcilerFor = mt.netpolFor
			netpolCtl.EachReconciler = func(ctx context.Context, fn func(*netpol.Reconciler) error) error {
				return eachTenant(ctx, mt, mt.netpolFor, fn)
			}
		}
		go func() {
			if err := netpolCtl.Run(ctx, 2); err != nil {
				vklog.G(ctx).WithError(err).Error(
					"the NetworkPolicy controller stopped; policies are no longer enforced")
			}
		}()
	}

	for _, spec := range specs {
		nodeObj := knode.Build(knode.Options{
			Name:   spec.name,
			Tenant: o.tenant,
			Shard:  o.shard,
			Zone:   spec.zone,
			// From the credential rather than a flag of its own: one set of
			// credentials resolves every service endpoint within one region
			// and cannot reach another, so the region is a property of the
			// process, not of the node. A flag could disagree with it.
			Region:     session.Region(),
			Arch:       spec.arch,
			Version:    version,
			InternalIP: o.internalIP,
			// The advertised kubelet port must follow the listen address:
			// the API server dials what the node advertises, and a node
			// listening on one port while advertising another has logs and
			// exec fail with a NotFound that looks like a missing pod.
			KubeletPort: kubeletPortOf(spec.listen),
			Capacity: knode.Capacity{
				CPU:              o.capacityCPU,
				Memory:           o.capacityMem,
				Pods:             o.capacityPod,
				EphemeralStorage: o.capacityEphemeral,
			},
		})
		nodeObj.Status.Conditions = []corev1.NodeCondition{knode.ReadyCondition()}

		p, err := provider.New(provider.Config{
			ServesNamespace:  set.Serves,
			Events:           set.EventRecorder("kubezun"),
			NetworkID:        o.networkID,
			AvailabilityZone: spec.zunAZ,
			Architecture:     spec.arch,
			NodeName:         spec.name,
			ClusterDomain:    o.clusterDomain,
			Tenant:           o.tenant,
			ClusterDNS:       splitList(o.clusterDNS),
			DNSService:       o.dnsService,
			NetworkIDFor: func() func(context.Context, string) (string, error) {
				if mt == nil {
					return nil
				}
				return mt.networkIDFor
			}(),
			ResolveClaim: func(namespace, claim string) (zun.ClaimMount, error) {
				if volRec == nil {
					return zun.ClaimMount{}, fmt.Errorf("persistent volumes are not enabled on this deployment")
				}
				m, err := volRec.MountFor(namespace, claim)
				if err != nil {
					return zun.ClaimMount{}, err
				}
				out := zun.ClaimMount{VolumeID: m.ID, Export: m.Export}
				switch m.Kind {
				case kvolume.Block:
					out.Kind = "cinder"
				case kvolume.Shared:
					out.Kind = "nfs"
				}
				return out, nil
			},
			Tokens: func(ctx context.Context, namespace, account string,
				req *authv1.TokenRequest) (*authv1.TokenRequest, error) {
				return client.CoreV1().ServiceAccounts(namespace).
					CreateToken(ctx, account, req, metav1.CreateOptions{})
			},
		}, providerCapsules(mt, capsuleAPI), provider.Caches{
			Pods:       set.PodsForNode(spec.name).Lister(),
			PodsSynced: set.PodsForNode(spec.name).Informer().HasSynced,
			Objects:    set.Objects(),
		})
		if err != nil {
			return err
		}
		// The provider is what knows where a pod's port is, and the reconciler
		// is what knows which groups belong on it. Neither can do the job
		// alone, and one reconciler serves every node this process runs.
		p.UsePolicies(netpolRec)

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
			NodeProvider: provider.NewNodeHealth(capsuleAPI, nodeObj),
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
		octavia, err := kservice.NewOctaviaClient(session)
		if err != nil {
			return fmt.Errorf("build the load balancer client: %w", err)
		}
		neutron, err := kservice.NewNetworkClient(session)
		if err != nil {
			return fmt.Errorf("build the network client: %w", err)
		}
		svcRec := &kservice.Reconciler{
			Octavia:           octavia,
			Neutron:           neutron,
			Subnets:           kservice.NewCapsuleSubnets(capsuleAPI),
			ServiceClient:     client.CoreV1(),
			VIPSubnetID:       o.vipSubnet,
			VIPNetworkID:      o.vipNetwork,
			FloatingNetworkID: o.floatingNet,
			PublicByDefault:   o.publicSvcs,
			ServesNamespace:   set.Serves,
			Namespaces:        set.ServedNamespaces,
			Events:            set.EventRecorder("service-controller"),
			Tenant:            o.tenant,
		}
		var controller *kservice.Controller
		if mt != nil {
			// The reconciler must read from the same source the events come
			// from, or reads and events disagree about which namespaces exist.
			svcRec.Services = mt.scoped.ServiceLister()
			svcRec.Slices = mt.scoped.EndpointSliceLister()
			controller, err = kservice.NewControllerFromSource(svcRec, mt.scoped)
		} else {
			svcRec.Services = set.ServiceInformer().Lister()
			svcRec.Slices = set.EndpointSliceInformer().Lister()
			controller, err = kservice.NewController(svcRec, set.ServiceInformer(), set.EndpointSliceInformer())
		}
		if err != nil {
			return err
		}
		if mt != nil {
			mt.service = svcRec
			controller.ReconcilerFor = mt.serviceFor
			controller.EachReconciler = func(ctx context.Context, fn func(*kservice.Reconciler) error) error {
				return eachTenant(ctx, mt, mt.serviceFor, fn)
			}
		}
		go controller.Run(ctx)
		// Load balancers of Services deleted while this was not running are
		// invisible to the queue; nothing but a sweep ever removes them.
		go controller.RunGC(ctx)
	} else {
		vklog.G(ctx).Warn("no --vip-subnet-id; Services get no load balancer, " +
			"and a pod cannot reach a Service by its cluster address")
	}

	if o.ingressProvider != "" && o.vipSubnet != "" {
		if o.ingressProvider == "ovn" {
			return fmt.Errorf("--ingress-provider=ovn cannot work: the OVN provider " +
				"is L4-only and refuses every L7 object; use amphora, or incus where " +
				"its driver serves L7")
		}
		octavia, err := kservice.NewOctaviaClient(session)
		if err != nil {
			return fmt.Errorf("build the load balancer client: %w", err)
		}
		neutron, err := kservice.NewNetworkClient(session)
		if err != nil {
			return fmt.Errorf("build the network client: %w", err)
		}
		// Barbican is optional: without it plain-HTTP Ingress still works and
		// TLS is refused with a readable error.
		keymanager, kmErr := kservice.NewKeyManagerClient(session)
		if kmErr != nil {
			vklog.G(ctx).WithError(kmErr).Warn(
				"no key-manager endpoint; TLS Ingress will be refused")
			keymanager = nil
		}
		ingRec := &kingress.Reconciler{
			Octavia:           octavia,
			Neutron:           neutron,
			KeyManager:        keymanager,
			IngressClient:     client.NetworkingV1(),
			Secrets:           set.Objects().Secret,
			Subnets:           kservice.NewCapsuleSubnets(capsuleAPI),
			VIPSubnetID:       o.vipSubnet,
			FloatingNetworkID: o.floatingNet,
			Provider:          o.ingressProvider,
			ClassName:         o.ingressClass,
			Tenant:            o.tenant,
			ServesNamespace:   set.Serves,
			Namespaces:        set.ServedNamespaces,
			Events:            set.EventRecorder("ingress-controller"),
		}
		var ingressCtl *kingress.Controller
		if mt != nil {
			ingRec.Ingresses = mt.scoped.IngressLister()
			ingRec.Services = mt.scoped.ServiceLister()
			ingRec.Slices = mt.scoped.EndpointSliceLister()
			ingressCtl, err = kingress.NewControllerFromSource(ingRec, mt.scoped)
		} else {
			ingRec.Ingresses = set.IngressInformer().Lister()
			ingRec.Services = set.ServiceInformer().Lister()
			ingRec.Slices = set.EndpointSliceInformer().Lister()
			ingressCtl, err = kingress.NewController(ingRec, set.IngressInformer(), set.EndpointSliceInformer())
		}
		if err != nil {
			return err
		}
		if mt != nil {
			mt.ingress = ingRec
			ingressCtl.ReconcilerFor = mt.ingressFor
			ingressCtl.EachReconciler = func(ctx context.Context, fn func(*kingress.Reconciler) error) error {
				return eachTenant(ctx, mt, mt.ingressFor, fn)
			}
		}
		go ingressCtl.Run(ctx)
		go ingressCtl.RunGC(ctx)
	} else if o.ingressProvider != "" {
		vklog.G(ctx).Warn("--ingress-provider is set but --vip-subnet-id is not; Ingress stays off")
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
func serverTLS(o options) (*tls.Config, *vkset.CertReloader, error) {
	if o.tlsCert == "" && o.tlsKey == "" {
		return nil, nil, nil
	}
	if o.tlsCert == "" || o.tlsKey == "" {
		return nil, nil, fmt.Errorf("--tls-cert-file and --tls-private-key-file must be given together")
	}
	if o.clientCA == "" {
		// Without it the server cannot tell the API server from anyone else who
		// can reach the port, and logs and exec reach into tenant containers.
		return nil, nil, fmt.Errorf(
			"--client-ca-file is required with a serving certificate: without it the " +
				"kubelet API cannot verify who is calling")
	}

	// Read per handshake rather than once, so a renewed certificate is picked
	// up while the process runs. A kubelet-serving certificate is short-lived,
	// and one that expires while its replacement sits unread on disk takes
	// logs and exec down with the node still Ready.
	reloader, err := vkset.NewCertReloader(o.tlsCert, o.tlsKey)
	if err != nil {
		return nil, nil, err
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		CipherSuites:   nodeutil.DefaultServerCiphers(),
		GetCertificate: reloader.GetCertificate,
	}
	if err := nodeutil.WithCAFromPath(o.clientCA)(cfg); err != nil {
		return nil, nil, fmt.Errorf("load client CA: %w", err)
	}
	// Ask for a client certificate; do not demand one. The helper above sets
	// RequireAndVerifyClientCert, which refuses at the TLS handshake anything
	// that authenticates another way -- and metrics-server authenticates with
	// a bearer token, as kubelet has always allowed. A real kubelet requests
	// rather than requires for exactly this reason.
	//
	// Nothing is given away by it. The certificate was never what made this
	// port safe: every request is still authenticated (x509 or TokenReview)
	// and authorized (SubjectAccessReview on nodes/<name>) before it reaches a
	// handler, and a caller with neither credential is refused there with 401.
	// What the strict setting bought was a slightly earlier "no" for one kind
	// of caller, at the price of "kubectl top" reporting <unknown> forever.
	cfg.ClientAuth = tls.RequestClientCert
	return cfg, reloader, nil
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

// convertTenant runs one half of the security group conversion and returns.
//
// A separate invocation rather than something the controller does on start:
// it changes every capsule of a tenant at once, the two halves must be run in
// order with the operator deciding when, and the first thing anyone will want
// is to see what it would do.
func convertTenant(ctx context.Context, session *tenant.Session, phase netpol.Phase, confirm bool) error {
	switch phase {
	case netpol.PhaseAttach, netpol.PhaseDetach:
	default:
		return fmt.Errorf("the conversion phase must be %q or %q, not %q",
			netpol.PhaseAttach, netpol.PhaseDetach, phase)
	}
	client, err := netpol.NewClient(session)
	if err != nil {
		return err
	}
	n := &netpol.Neutron{Client: client}
	ingress, egress, err := n.EnsureBaseline(ctx)
	if err != nil {
		return fmt.Errorf("preparing the baseline groups: %w", err)
	}
	denyAll, err := n.EnsureDenyAll(ctx)
	if err != nil {
		return err
	}

	changed, total, err := n.Convert(ctx, phase, []string{denyAll, ingress, egress}, !confirm)
	if err != nil {
		return err
	}
	// Printed rather than logged: this runs before the logger is configured,
	// and its output is the whole point of the command -- an operator deciding
	// whether to let it write.
	if confirm {
		fmt.Printf("%s: changed %d of %d capsule ports\n", phase, changed, total)
		if phase == netpol.PhaseAttach {
			fmt.Println("every port is now at least as reachable as it was. " +
				"Run the detach phase only once this has been done for the " +
				"whole tenant.")
		}
		return nil
	}
	fmt.Printf("%s: would change %d of %d capsule ports; nothing was written\n",
		phase, changed, total)
	fmt.Println("pass --convert-confirm to apply")
	return nil
}

// placementOf answers where a node this process runs puts its capsules.
//
// Read from the node specifications rather than from the node object's labels:
// the two names for a zone -- the one Kubernetes is told and the one OpenStack
// is asked for -- only appear together here, in the flags that set both. A
// claim scheduled onto a node we do not run gets no answer, and falls back to
// whatever the deployment configured.
func placementOf(specs []nodeSpec, region string) func(string) (kvolume.Placement, bool) {
	where := make(map[string]kvolume.Placement, len(specs))
	for _, spec := range specs {
		where[spec.name] = kvolume.Placement{Zone: spec.zone, AZ: spec.zunAZ, Region: region}
	}
	return func(node string) (kvolume.Placement, bool) {
		p, ok := where[node]
		return p, ok
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

// splitList reads a comma-separated flag, dropping empties so a trailing comma
// or an unset variable does not become an address of "".
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// kubeletPortOf reads the port out of a listen address, for the node to
// advertise. Zero lets the node default stand (10250).
func kubeletPortOf(listen string) int32 {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}
