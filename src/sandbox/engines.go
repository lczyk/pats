package sandbox

import "sort"

// engine is what pats needs of a container engine's cli where engines
// differ, nearly all of it in egress network setup: one entry per engine, so a new
// one (nerdctl) is an entry and a ci job.
type engine struct {
	// defaultNetwork is the engine's network with a way out.
	defaultNetwork string
	// internalNet are the create flags an internal network needs on top of
	// --internal, to be one the agent can only leave through the proxy.
	internalNet []string
	// agentNet are the run flags a container on that network needs.
	agentNet []string
	// runFlags go on every agent and sidecar run: what keeps the engine from
	// adding env of its own.
	runFlags []string
	// cpus is the info template for the daemon's cpu count.
	cpus string
}

var engines = map[string]engine{
	"docker": {
		defaultNetwork: "bridge",
		// docker gives the host an address on the bridge, which the agent
		// could reach any host service through: --internal cuts only routes.
		internalNet: []string{"-o", "com.docker.network.bridge.inhibit_ipv4=true"},
		// docker's embedded resolver on an internal network may still forward
		// external names to the host; with no upstream it cannot. peer names
		// (none needed, the proxy is an ip) keep resolving.
		agentNet: []string{"--dns", "0.0.0.0"},
		cpus:     "{{.NCPU}}",
	},
	"podman": {
		defaultNetwork: "podman",
		// podman's resolver on an internal network answers only for peers,
		// and the sidecar would inherit it as its only nameserver: it could
		// then resolve nothing upstream. nothing here needs peer names.
		internalNet: []string{"--disable-dns"},
		// podman copies the host's proxy variables into every container; the
		// proxy env a sandbox gets is pats's to set.
		runFlags: []string{"--http-proxy=false"},
		cpus:     "{{.Host.CPUs}}",
	},
}

// IsEngine says whether driver names a container engine pats drives.
func IsEngine(driver string) bool {
	_, ok := engines[driver]
	return ok
}

// Engines are the container engines pats drives, by driver name.
func Engines() []string {
	out := make([]string, 0, len(engines))
	for name := range engines {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
