package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the contract tests of the container engines: each thing the driver reads
// out of docker's or podman's words, and fakeDaemon answers in its stead, is
// one scenario, run against the fake in unit tests and against the real cli
// when its daemon answers. an engine release that rewords one fails here,
// not in a run. the lxd counterpart is lxd_contract_test.go.

// engineEnv is an engine a scenario runs against.
type engineEnv struct {
	r      runner
	driver string
	tag    string // unique to the run, in every name a scenario makes
	// net is what a network create is given besides its name: the fake
	// wants the subnet the driver always names, a real daemon picks one.
	net []string
}

func (c *engineEnv) cli(args ...string) (string, error) {
	return combined(context.Background(), c.r, args...)
}

// said is a failed call as the driver sees it: the error with the cli's words.
func said(out string, err error) error {
	if err == nil {
		return nil
	}
	return errors.New(err.Error() + ": " + out)
}

var engineContracts = []struct {
	name string
	run  func(t *testing.T, c *engineEnv)
}{
	{"an image that is not there says so", func(t *testing.T, c *engineEnv) {
		out, err := c.cli("image", "inspect", "--format", "{{.Id}}", "pats-ct-"+c.tag+":none")
		require.That(t, err != nil, "an image nobody made was found: %s", out)
		assert.That(t, notFound(said(out, err)), "not read as a missing image: %s", out)
	}},
	{"a network name is taken once, and the second taker is told", func(t *testing.T, c *engineEnv) {
		name := "pats-ct-" + c.tag
		create := append(append([]string{"network", "create"}, c.net...), name)
		out, err := c.cli(create...)
		require.NoError(t, err, "%s", out)
		t.Cleanup(func() { _, _ = c.cli("network", "rm", name) })
		out, err = c.cli(create...)
		require.That(t, err != nil, "a second network of one name was made: %s", out)
		assert.That(t, nameTaken(out), "not read as a name taken: %s", out)
	}},
	{"a network that is not there is gone, said or not", func(t *testing.T, c *engineEnv) {
		out, err := c.cli("network", "rm", "pats-ct-"+c.tag+"-none")
		assert.That(t, err == nil || gone(said(out, err)), "not read as gone: %s", out)
	}},
	{"a container that is not there is gone, said or not", func(t *testing.T, c *engineEnv) {
		out, err := c.cli("rm", "-f", "pats-ct-"+c.tag+"-none")
		assert.That(t, err == nil || gone(said(out, err)), "not read as gone: %s", out)
	}},
	{"the cpu count is a number", func(t *testing.T, c *engineEnv) {
		out, err := c.cli("info", "--format", engines[c.driver].cpus)
		require.NoError(t, err, "%s", out)
		n, err := strconv.Atoi(strings.TrimSpace(out))
		require.NoError(t, err, "%q", out)
		assert.That(t, n > 0, "%d cpus", n)
	}},
	{"every image is listed by its whole id", func(t *testing.T, c *engineEnv) {
		out, err := c.cli("images", "-a", "-q", "--no-trunc")
		require.NoError(t, err, "%s", out)
		for _, id := range strings.Fields(out) {
			h := strings.TrimPrefix(id, "sha256:")
			assert.That(t, len(h) == 64 && isHex(h), "%q is no whole image id", id)
		}
	}},
}

func TestEngineContractFake(t *testing.T) {
	for _, driver := range Engines() {
		for _, sc := range engineContracts {
			t.Run(driver+"/"+sc.name, func(t *testing.T) {
				f := newFakeDaemon(t, driver)
				sc.run(t, &engineEnv{r: runFunc(f.run), driver: driver, tag: contractTag(), net: []string{"--subnet", "10.213.7.0/24"}})
			})
		}
	}
}

// TestEngineContract runs the scenarios against each engine whose daemon
// answers on this machine.
func TestEngineContract(t *testing.T) {
	for _, driver := range Engines() {
		t.Run(driver, func(t *testing.T) {
			if _, err := exec.LookPath(driver); err != nil {
				t.Skip(driver + " not on PATH")
			}
			if err := exec.Command(driver, "info").Run(); err != nil {
				t.Skip(driver + " not responding")
			}
			for _, sc := range engineContracts {
				t.Run(sc.name, func(t *testing.T) {
					sc.run(t, &engineEnv{r: engineRunner(Options{Driver: driver}), driver: driver, tag: contractTag()})
				})
			}
		})
	}
}
