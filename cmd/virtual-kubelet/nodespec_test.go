package main

import "testing"

func TestParseNodeSpecFillsFromDefaults(t *testing.T) {
	defaults := nodeSpec{arch: "amd64", zone: "az1", zunAZ: "nova", listen: ":10250"}

	got, err := parseNodeSpec("name=t1-arm,arch=arm64,listen=:10251", defaults)
	if err != nil {
		t.Fatalf("parseNodeSpec: %v", err)
	}
	if got.name != "t1-arm" || got.arch != "arm64" || got.listen != ":10251" {
		t.Errorf("stated fields not taken: %+v", got)
	}
	// Unstated fields come from the shared flags rather than from zero values:
	// an empty zunAZ would let Zun place the capsule in any zone, silently
	// undoing the operator's placement.
	if got.zone != "az1" || got.zunAZ != "nova" {
		t.Errorf("unstated fields not defaulted: %+v", got)
	}
}

func TestParseNodeSpecRefusesUnknownFields(t *testing.T) {
	// Ignoring a misspelled key would leave the node on the default instead —
	// for arch, that means capsules placed on a machine that cannot run them.
	for _, bad := range []string{"name=n,architecture=arm64", "name=n,az=nova", "arch=arm64", "name=n,oops"} {
		if _, err := parseNodeSpec(bad, nodeSpec{}); err == nil {
			t.Errorf("parseNodeSpec(%q) was accepted", bad)
		}
	}
}

func TestNodeSpecsRejectsCollisions(t *testing.T) {
	base := options{nodeName: "t1-az1", arch: "amd64", listenAddr: ":10250"}

	// Two controllers on one node object would fight over its status and each
	// treat the other's pods as its own.
	dup := base
	dup.nodes = nodeSpecList{"name=t1-az1,arch=arm64,listen=:10251"}
	if _, err := nodeSpecs(dup); err == nil {
		t.Error("a node named twice was accepted")
	}

	// Only one can bind the address; the loser serves no kubelet API and its
	// logs and exec fail with nothing saying why.
	addr := base
	addr.nodes = nodeSpecList{"name=t1-arm,arch=arm64"}
	if _, err := nodeSpecs(addr); err == nil {
		t.Error("two nodes sharing a listen address were accepted")
	}

	// An architecture no host can report would register a node whose label
	// nothing satisfies, leaving its pods Pending with no explanation.
	arch := base
	arch.nodes = nodeSpecList{"name=t1-arm,arch=aarch64,listen=:10251"}
	if _, err := nodeSpecs(arch); err == nil {
		t.Error("an unknown architecture was accepted")
	}

	ok := base
	ok.nodes = nodeSpecList{"name=t1-arm,arch=arm64,listen=:10251"}
	specs, err := nodeSpecs(ok)
	if err != nil {
		t.Fatalf("nodeSpecs: %v", err)
	}
	if len(specs) != 2 || specs[0].name != "t1-az1" || specs[1].arch != "arm64" {
		t.Errorf("unexpected specs: %+v", specs)
	}
}

func TestNodeSpecsRequiresANode(t *testing.T) {
	if _, err := nodeSpecs(options{}); err == nil {
		t.Error("a process with no nodes was accepted")
	}
}
