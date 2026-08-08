// Package vknode runs several virtual nodes for one tenant inside a single
// process, off one set of informers.
//
// nodeutil.NewNode builds its own informer factories per node, so N nodes meant
// N pod watches and N cluster-wide secret/configmap/service watches. A tenant
// needs a node per architecture and per availability zone, so N is a product
// knob (DESIGN §3.4/§3.6) and that cost is paid on every one. This package
// assembles the same controllers directly against shared informers instead.
package vknode

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corev1informers "k8s.io/client-go/informers/core/v1"
	discoveryv1informers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/fivetime/kubezun/pkg/provider"
)

// Set holds what every node in the process shares: the client, the informers,
// and the event broadcaster.
type Set struct {
	client kubernetes.Interface

	podFactory informers.SharedInformerFactory
	scmFactory informers.SharedInformerFactory

	pods       corev1informers.PodInformer
	secrets    corev1informers.SecretInformer
	configMaps corev1informers.ConfigMapInformer
	services   corev1informers.ServiceInformer
	slices     discoveryv1informers.EndpointSliceInformer

	// namespaces is the watched set this process serves. Nil falls back to
	// fixed, the list given at startup.
	namespaces *Namespaces
	fixed      []string

	broadcaster   record.EventBroadcaster
	workers       int
	leaseDuration int

	nodes []*Node
}

// SetOptions describes the shared half of a tenant's virtual nodes.
type SetOptions struct {
	Client kubernetes.Interface
	// Namespaces the process serves. When exactly one is given the pod watch is
	// scoped to it, which is both less traffic and less privilege than watching
	// the cluster. With several, the watch has to span namespaces, and each
	// node's own event filter plus the provider's namespace check are what keep
	// other tenants' pods out.
	Namespaces []string
	// NamespaceWatcher supplies the served set when it is watched rather than
	// configured. Nil keeps Namespaces as the whole answer.
	NamespaceWatcher *Namespaces
	// ResyncPeriod for the shared informers. Zero means the client-go default.
	ResyncPeriod time.Duration
	// Workers per pod controller. Zero means one.
	Workers int

	// LeaseDurationSeconds is how long a node's lease is valid, and so how
	// often it is renewed. Zero takes virtual-kubelet's default, which is a
	// real kubelet's.
	//
	// A longer one than a real kubelet's is defensible here and is the single
	// largest lever on what a virtual node costs the control plane: a physical
	// machine can stop answering between one heartbeat and the next, where a
	// virtual node's health is the health of a process that is either running
	// or not. The cost is how long the scheduler keeps placing pods on a node
	// whose process has died (DESIGN §3.5).
	LeaseDurationSeconds int
}

// NewSet builds the shared informers for a process's virtual nodes.
func NewSet(opts SetOptions) (*Set, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("a Kubernetes client is required")
	}
	if len(opts.Namespaces) == 0 {
		return nil, fmt.Errorf("at least one namespace must be served")
	}
	if opts.ResyncPeriod == 0 {
		opts.ResyncPeriod = time.Minute
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}

	podOpts := []informers.SharedInformerOption{}
	if len(opts.Namespaces) == 1 {
		podOpts = append(podOpts, informers.WithNamespace(opts.Namespaces[0]))
	}
	// No spec.nodeName field selector: one informer serves every node in the
	// process, and each pod controller is given a filter naming its own node
	// (PodEventFilterFunc, which exists for exactly this).
	podFactory := informers.NewSharedInformerFactoryWithOptions(
		opts.Client, opts.ResyncPeriod, podOpts...)

	// Secrets, configmaps and services are read to resolve a pod's environment,
	// which can name any object in the pod's own namespace, so this factory is
	// scoped the same way.
	scmOpts := []informers.SharedInformerOption{}
	if len(opts.Namespaces) == 1 {
		scmOpts = append(scmOpts, informers.WithNamespace(opts.Namespaces[0]))
	}
	scmFactory := informers.NewSharedInformerFactoryWithOptions(
		opts.Client, opts.ResyncPeriod, scmOpts...)

	return &Set{
		client:        opts.Client,
		fixed:         opts.Namespaces,
		namespaces:    opts.NamespaceWatcher,
		podFactory:    podFactory,
		scmFactory:    scmFactory,
		pods:          podFactory.Core().V1().Pods(),
		secrets:       scmFactory.Core().V1().Secrets(),
		configMaps:    scmFactory.Core().V1().ConfigMaps(),
		services:      scmFactory.Core().V1().Services(),
		slices:        scmFactory.Discovery().V1().EndpointSlices(),
		workers:       opts.Workers,
		leaseDuration: opts.LeaseDurationSeconds,
	}, nil
}

// EventRecorder returns a recorder for a component sharing this set's
// broadcaster, so events reach the API server through the same path the pod
// controllers use.
func (s *Set) EventRecorder(component string) record.EventRecorder {
	if s.broadcaster == nil {
		s.broadcaster = record.NewBroadcaster()
	}
	return s.broadcaster.NewRecorder(scheme.Scheme,
		corev1.EventSource{Component: component})
}

// Pods is the shared pod lister, spanning every node in the process rather than
// only one node's pods.
func (s *Set) Pods() corev1listers.PodLister { return s.pods.Lister() }

// ConfigMaps and Secrets are the shared listers a provider reads a pod's file
// volumes from.
func (s *Set) ConfigMaps() corev1listers.ConfigMapLister { return s.configMaps.Lister() }
func (s *Set) Secrets() corev1listers.SecretLister       { return s.secrets.Lister() }

// ServiceInformer and EndpointSliceInformer back the Service controller, which
// runs once per process rather than once per node: load balancers belong to the
// tenant, not to any one of its virtual nodes.
func (s *Set) ServiceInformer() corev1informers.ServiceInformer { return s.services }
func (s *Set) EndpointSliceInformer() discoveryv1informers.EndpointSliceInformer {
	return s.slices
}

// Node is one virtual node: its node controller, its pod controller, and its
// kubelet API endpoint.
type Node struct {
	name string

	nc *vknode.NodeController
	pc *vknode.PodController

	provider nodeutil.Provider
	pods     corev1listers.PodLister

	handler               http.Handler
	auth                  nodeutil.Auth
	tlsConfig             *tls.Config
	listenAddr            string
	streamIdleTimeout     time.Duration
	streamCreationTimeout time.Duration

	workers int
	ready   chan struct{}
	done    chan struct{}
	err     error
}

// NodeOptions describes one virtual node.
type NodeOptions struct {
	// Spec is the node object to register. It is registered as given.
	Spec *corev1.Node
	// Provider runs this node's pods and serves its kubelet API.
	Provider nodeutil.Provider
	// NodeProvider reports this node's status. Required: the default one keeps
	// a node Ready whatever happens to its backend.
	NodeProvider vknode.NodeProvider
	// ListenAddr for the kubelet API. The server only runs with TLSConfig set;
	// without it logs and exec have no endpoint, which is what nodeutil does
	// too.
	ListenAddr string
	TLSConfig  *tls.Config
	// Handler serves the kubelet API. When nil and TLSConfig is set, the
	// standard pod routes are attached.
	Handler http.Handler
	// Auth authenticates and authorizes kubelet API requests. Its authorizer
	// names this node, so each node needs its own — which is why each also
	// needs its own listen address. Without it the endpoint would serve any
	// caller that completes the TLS handshake.
	Auth nodeutil.Auth
	// StreamIdleTimeout and StreamCreationTimeout bound exec and attach streams.
	StreamIdleTimeout     time.Duration
	StreamCreationTimeout time.Duration
}

// AddNode registers a virtual node with the set. It is not running until the
// set is.
func (s *Set) AddNode(opts NodeOptions) (*Node, error) {
	if opts.Spec == nil || opts.Spec.Name == "" {
		return nil, fmt.Errorf("a node spec with a name is required")
	}
	if opts.Provider == nil {
		return nil, fmt.Errorf("node %s: a provider is required", opts.Spec.Name)
	}
	if opts.NodeProvider == nil {
		// Not defaulted to NaiveNodeProvider on purpose: that one holds a node
		// Ready no matter what, so the scheduler would keep sending pods to a
		// node whose backend is unreachable.
		return nil, fmt.Errorf("node %s: a node provider is required", opts.Spec.Name)
	}

	name := opts.Spec.Name

	leaseSeconds := int32(s.leaseDuration)
	if leaseSeconds <= 0 {
		leaseSeconds = vknode.DefaultLeaseDuration
	}

	if s.broadcaster == nil {
		s.broadcaster = record.NewBroadcaster()
	}
	recorder := s.broadcaster.NewRecorder(scheme.Scheme,
		corev1.EventSource{Component: path.Join(name, "pod-controller")})

	nc, err := vknode.NewNodeController(
		opts.NodeProvider,
		opts.Spec,
		s.client.CoreV1().Nodes(),
		vknode.WithNodeEnableLeaseV1(
			s.client.CoordinationV1().Leases(corev1.NamespaceNodeLease),
			leaseSeconds),
	)
	if err != nil {
		return nil, fmt.Errorf("node %s: node controller: %w", name, err)
	}

	pc, err := vknode.NewPodController(vknode.PodControllerConfig{
		PodClient:         s.client.CoreV1(),
		EventRecorder:     recorder,
		Provider:          opts.Provider,
		PodInformer:       s.pods,
		SecretInformer:    s.secrets,
		ConfigMapInformer: s.configMaps,
		ServiceInformer:   s.services,
		// The shared informer carries every node's pods, so this is what keeps
		// one node's controller from acting on another's. Without it each node
		// would try to create a capsule for every pod in the namespace.
		PodEventFilterFunc: func(_ context.Context, pod *corev1.Pod) bool {
			return pod.Spec.NodeName == name
		},
	})
	if err != nil {
		return nil, fmt.Errorf("node %s: pod controller: %w", name, err)
	}

	n := &Node{
		name:                  name,
		nc:                    nc,
		pc:                    pc,
		handler:               opts.Handler,
		auth:                  opts.Auth,
		tlsConfig:             opts.TLSConfig,
		listenAddr:            opts.ListenAddr,
		workers:               s.workers,
		ready:                 make(chan struct{}),
		done:                  make(chan struct{}),
		streamIdleTimeout:     opts.StreamIdleTimeout,
		streamCreationTimeout: opts.StreamCreationTimeout,
		provider:              opts.Provider,
		pods:                  s.pods.Lister(),
	}
	s.nodes = append(s.nodes, n)
	return n, nil
}

// Run starts the shared informers and every node registered on the set. It
// returns when the first node stops or the context is cancelled.
func (s *Set) Run(ctx context.Context) error {
	if len(s.nodes) == 0 {
		return fmt.Errorf("no nodes were registered")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if s.broadcaster != nil {
		s.broadcaster.StartLogging(log.G(ctx).Infof)
		s.broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{
			Interface: s.client.CoreV1().Events(corev1.NamespaceAll)})
		defer s.broadcaster.Shutdown()
	}

	// Started once for the whole process rather than once per node: that
	// saving is the reason this package exists.
	go s.podFactory.Start(ctx.Done())
	go s.scmFactory.Start(ctx.Done())

	errs := make(chan error, len(s.nodes))
	for _, n := range s.nodes {
		go func(n *Node) { errs <- n.run(ctx) }(n)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errs:
		return err
	}
}

// WaitReady blocks until every node in the set has registered and reported
// ready.
func (s *Set) WaitReady(ctx context.Context) error {
	for _, n := range s.nodes {
		select {
		case <-n.ready:
		case <-n.done:
			return fmt.Errorf("node %s stopped before becoming ready: %w", n.name, n.err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Name reports the node's registered name.
func (n *Node) Name() string { return n.name }

func (n *Node) run(ctx context.Context) (retErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		n.err = retErr
		close(n.done)
	}()

	stopHTTP, err := n.runHTTP(ctx)
	if err != nil {
		return err
	}
	defer stopHTTP()

	go n.pc.Run(ctx, n.workers) //nolint:errcheck
	defer func() {
		cancel()
		<-n.pc.Done()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-n.pc.Ready():
	case <-n.pc.Done():
		return n.pc.Err()
	}
	log.G(ctx).WithField("node", n.name).Debug("pod controller ready")

	go n.nc.Run(ctx) //nolint:errcheck
	defer func() {
		cancel()
		<-n.nc.Done()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-n.nc.Ready():
	case <-n.nc.Done():
		return n.nc.Err()
	}
	log.G(ctx).WithField("node", n.name).Debug("node controller ready")

	close(n.ready)

	select {
	case <-n.nc.Done():
		return n.nc.Err()
	case <-n.pc.Done():
		return n.pc.Err()
	}
}

func (n *Node) runHTTP(ctx context.Context) (func(), error) {
	if n.tlsConfig == nil {
		// The API server will not talk to a kubelet endpoint over plain HTTP,
		// so serving one would only invite unauthenticated access to logs and
		// exec. Same call nodeutil makes.
		log.G(ctx).WithField("node", n.name).
			Warn("no TLS config; the kubelet API is not served, so logs and exec are unavailable")
		return func() {}, nil
	}
	handler := n.handler
	if handler == nil {
		mux := http.NewServeMux()
		mux.Handle("/", api.PodHandler(api.PodHandlerConfig{
			RunInContainer:    n.provider.RunInContainer,
			AttachToContainer: n.provider.AttachToContainer,
			GetContainerLogs:  n.provider.GetContainerLogs,
			GetPods:           n.provider.GetPods,
			GetPodsFromKubernetes: func(context.Context) ([]*corev1.Pod, error) {
				return n.pods.List(labels.Everything())
			},
			GetStatsSummary:       n.provider.GetStatsSummary,
			GetMetricsResource:    n.provider.GetMetricsResource,
			StreamIdleTimeout:     n.streamIdleTimeout,
			StreamCreationTimeout: n.streamCreationTimeout,
			PortForward:           n.provider.PortForward,
		}, true))
		handler = mux
	}
	if n.auth != nil {
		handler = nodeutil.WithAuth(n.auth, handler)
	} else {
		// Logs and exec reach into a tenant's containers, so an endpoint that
		// authorizes nobody is worth refusing rather than running.
		return nil, fmt.Errorf(
			"node %s: TLS is configured but no authorizer; refusing to serve the "+
				"kubelet API, which would expose logs and exec to any caller", n.name)
	}

	l, err := tls.Listen("tcp", n.listenAddr, n.tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("node %s: kubelet API listener: %w", n.name, err)
	}
	srv := &http.Server{
		Handler:           handler,
		TLSConfig:         n.tlsConfig,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go srv.Serve(l) //nolint:errcheck

	return func() {
		srv.Close() //nolint:errcheck
		l.Close()   //nolint:errcheck
	}, nil
}

// Serves reports whether a namespace is one this process runs pods for. It is
// the provider's authorization boundary, so it is answered from the watched set
// rather than from anything fixed at startup.
func (s *Set) Serves(namespace string) bool {
	if s.namespaces == nil {
		// No watcher configured: fall back to the fixed list this process was
		// started with, which is still a closed set.
		for _, n := range s.fixed {
			if n == namespace {
				return true
			}
		}
		return false
	}
	return s.namespaces.Serves(namespace)
}

// Objects reads a pod's ConfigMaps and Secrets on demand, without caching them.
func (s *Set) Objects() provider.ObjectReader { return objectReader{client: s.client} }
