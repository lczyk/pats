package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
)

func init() {
	registerEngine("podman", engine{
		defaultNetwork: "podman",
		// podman's resolver on an internal network answers only for peers,
		// and the sidecar would inherit it as its only nameserver: it could
		// then resolve nothing upstream. nothing here needs peer names.
		internalNet: []string{"--disable-dns"},
		// podman copies the host's proxy variables into every container; the
		// proxy env a sandbox gets is pats's to set.
		runFlags: []string{"--http-proxy=false"},
		cpus:     "{{.Host.CPUs}}",
		target:   podmanTarget,
		env:      podmanEnv,
	})
}

// podmanTarget is podman:<connection> with the connection's uri (podman has
// no daemon id), podman:local when it has no connection configured.
func podmanTarget(ctx context.Context, c *container) (Target, error) {
	// the cli's own overrides first: a service uri, then a named connection.
	if uri := c.o.ContainerHost; uri != "" {
		return Target{Key: "podman:" + uri, Daemon: uri}, nil
	}
	want := c.o.ContainerConnection
	out, err := c.dockerOut(ctx, "system", "connection", "list", "--format", "json")
	if err != nil {
		return Target{}, err
	}
	var conns []struct {
		Name    string
		URI     string
		Default bool
	}
	_ = json.Unmarshal(out, &conns)
	for _, cn := range conns {
		if (want != "" && cn.Name == want) || (want == "" && cn.Default) {
			return Target{Key: "podman:" + cn.Name, Daemon: cn.URI}, nil
		}
	}
	if want != "" {
		return Target{}, fmt.Errorf("podman connection %q (CONTAINER_CONNECTION) not found", want)
	}
	return Target{Key: "podman:local", Daemon: "local"}, nil
}

// podmanEnv aims podman's cli as o says, through the environment it reads.
func podmanEnv(o Options) []string {
	var env []string
	if o.ContainerHost != "" {
		env = append(env, "CONTAINER_HOST="+o.ContainerHost)
	}
	if o.ContainerConnection != "" {
		env = append(env, "CONTAINER_CONNECTION="+o.ContainerConnection)
	}
	return env
}
