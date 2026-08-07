package main

import (
	"fmt"
	"strings"
)

// nodeSpec is one virtual node named on the command line.
//
// A tenant needs a node per architecture and per availability zone (DESIGN
// §3.4/§3.6), and running each in its own process multiplies the informers, the
// watches and the credentials by the number of nodes. So the process takes a
// list: the fields that differ between a tenant's nodes live here, and
// everything its nodes share stays a plain flag.
type nodeSpec struct {
	name string
	arch string
	// zone is the Kubernetes topology label; zunAZ is the OpenStack
	// availability zone. They are different namespaces of names — assuming they
	// matched once made every capsule fail to schedule — so they stay separate.
	zone   string
	zunAZ  string
	listen string
}

// parseNodeSpec reads "name=<n>[,arch=<a>][,zone=<z>][,zun-az=<az>][,listen=<addr>]",
// filling anything unstated from defaults.
func parseNodeSpec(s string, defaults nodeSpec) (nodeSpec, error) {
	out := defaults
	out.name = ""

	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return nodeSpec{}, fmt.Errorf("node spec %q: field %q is not key=value", s, field)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "name":
			out.name = value
		case "arch":
			out.arch = value
		case "zone":
			out.zone = value
		case "zun-az":
			out.zunAZ = value
		case "listen":
			out.listen = value
		default:
			// Refused rather than ignored: a misspelled key would leave the node
			// running with the default instead, and for arch that means capsules
			// placed on a machine that cannot run their image.
			return nodeSpec{}, fmt.Errorf(
				"node spec %q: unknown field %q; want name, arch, zone, zun-az or listen", s, key)
		}
	}
	if out.name == "" {
		return nodeSpec{}, fmt.Errorf("node spec %q: name is required", s)
	}
	return out, nil
}

// nodeSpecList collects repeated --node flags.
type nodeSpecList []string

func (l *nodeSpecList) String() string { return strings.Join(*l, " ") }

func (l *nodeSpecList) Set(v string) error {
	*l = append(*l, v)
	return nil
}
