package sandbox

import (
	"context"
	"sort"
)

// engine is what pats needs of a container engine's cli where engines
// differ, nearly all of it in egress network setup: one entry per engine,
// each registered from a file of its own, so a new one (nerdctl) is a file and
// a ci job, and a build tag can leave one out.
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
	// target, when set, is the place's pin target; nil is docker's context
	// and daemon id.
	target func(ctx context.Context, c *container) (Target, error)
	// env, when set, is what aims the engine's cli where o says, as the
	// environment it reads.
	env func(o Options) []string
}

var engines = map[string]engine{}

// registerEngine adds an engine and its driver.
func registerEngine(name string, e engine) {
	engines[name] = e
	registerDriver(name, driverDef{
		open: func(p *process, r runner, o Options) Driver {
			if r == nil {
				r = engineRunner(o)
			}
			return newContainer(p, r, o)
		},
		reopen: reopenSame,
	})
}

func init() {
	registerEngine("docker", engine{
		defaultNetwork: "bridge",
		// docker gives the host an address on the bridge, which the agent
		// could reach any host service through: --internal cuts only routes.
		internalNet: []string{"-o", "com.docker.network.bridge.inhibit_ipv4=true"},
		// docker's embedded resolver on an internal network may still forward
		// external names to the host; with no upstream it cannot. peer names
		// (none needed, the proxy is an ip) keep resolving.
		agentNet: []string{"--dns", "0.0.0.0"},
		cpus:     "{{.NCPU}}",
	})
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
