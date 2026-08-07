package service

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func svcWith(t corev1.ServiceType, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "t1", Annotations: annotations},
		Spec:       corev1.ServiceSpec{Type: t},
	}
}

// A public address costs the platform money and exposes a service its author
// may only have meant to reach from inside, so it is never the answer to
// silence.
func TestPublicAddressIsNotTheDefault(t *testing.T) {
	if wantsPublicAddress(svcWith(corev1.ServiceTypeLoadBalancer, nil), false) {
		t.Error("a LoadBalancer Service saying nothing was given a public address")
	}
	// A ClusterIP Service still gets a load balancer, but never a public
	// address: type is the only thing a tenant writes that asks for one.
	if wantsPublicAddress(svcWith(corev1.ServiceTypeClusterIP, nil), true) {
		t.Error("a ClusterIP Service was given a public address")
	}
}

func TestAnnotationOverridesTheDefaultBothWays(t *testing.T) {
	askedPublic := svcWith(corev1.ServiceTypeLoadBalancer,
		map[string]string{InternalAnnotation: "false"})
	if !wantsPublicAddress(askedPublic, false) {
		t.Error("a Service that explicitly asked for a public address did not get one")
	}

	// The opt-out has to win over a platform that defaults to public, or a
	// tenant could not keep a service private.
	askedPrivate := svcWith(corev1.ServiceTypeLoadBalancer,
		map[string]string{InternalAnnotation: "true"})
	if wantsPublicAddress(askedPrivate, true) {
		t.Error("a Service that asked to stay internal was given a public address")
	}
}

// The annotation is the one cloud-provider-openstack uses, so a chart written
// for an OpenStack cluster behaves the same here rather than silently
// differently.
func TestAnnotationMatchesTheOpenStackConvention(t *testing.T) {
	if InternalAnnotation != "service.beta.kubernetes.io/openstack-internal-load-balancer" {
		t.Errorf("annotation is %q, which no existing chart carries", InternalAnnotation)
	}
}
