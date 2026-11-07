package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"
)

// the contract tests: each behaviour of lxd the driver relies on and
// fakeServer encodes is one scenario, run against the fake in unit tests and
// against a real server (the cli's default remote) under PATS_LXD_E2E=1. a
// newer lxd that changes one fails the contract run, not a production run.
// see docs/spec/lxd-testing.md, seam 8.

// contractEnv is a server a scenario runs against.
type contractEnv struct {
	r      runner
	remote string
	image  string            // what an instance is made from
	pool   string            // the storage pool instances get their root disk from
	tag    string            // unique to the run, in every name a scenario makes
	slow   func(name string) // makes the next create of name slow, where it can
}

func (c *contractEnv) lxc(args ...string) (string, error) {
	return combined(context.Background(), c.r, args...)
}

func (c *contractEnv) must(t *testing.T, args ...string) string {
	t.Helper()
	out, err := c.lxc(args...)
	require.NoError(t, err, "lxc %s: %s", strings.Join(args, " "), out)
	return out
}

// later runs a removal when the test ends, whatever happened.
func (c *contractEnv) later(t *testing.T, args ...string) {
	t.Cleanup(func() { _, _ = c.lxc(args...) })
}

func (c *contractEnv) ref(name string) string { return c.remote + ":" + name }

var lxdContracts = []struct {
	name string
	run  func(t *testing.T, c *contractEnv)
}{
	{"a missing instance says not found", func(t *testing.T, c *contractEnv) {
		out, err := c.lxc("delete", c.ref("pats-ct-"+c.tag+"-none"), "--project", lxdProject, "--force")
		require.Error(t, err, "")
		assert.ContainsString(t, strings.ToLower(out), "not found")
	}},
	{"a query in a missing project says not found", func(t *testing.T, c *contractEnv) {
		out, err := c.lxc("query", c.remote+":/1.0/instances?project=pats-ct-"+c.tag+"&recursion=1")
		require.Error(t, err, "")
		assert.ContainsString(t, strings.ToLower(out), "not found")
	}},
	{"an acl name is taken once", func(t *testing.T, c *contractEnv) {
		acl := "pats-ct-" + c.tag
		c.must(t, "network", "acl", "create", c.ref(acl), "--project", "default", "user.pats.run=ct")
		c.later(t, "network", "acl", "delete", c.ref(acl), "--project", "default")
		_, err := c.lxc("network", "acl", "create", c.ref(acl), "--project", "default")
		assert.Error(t, err, "")
		c.must(t, "network", "acl", "show", c.ref(acl), "--project", "default")
	}},
	{"what is in use cannot be deleted", func(t *testing.T, c *contractEnv) {
		acl, br, inst := "pats-ct-"+c.tag, "pct"+c.tag, "pats-ct-"+c.tag
		c.must(t, "network", "acl", "create", c.ref(acl), "--project", "default")
		c.later(t, "network", "acl", "delete", c.ref(acl), "--project", "default")
		c.must(t, "network", "create", c.ref(br), "--project", "default", "ipv4.address=none", "ipv6.address=none", "security.acls="+acl)
		c.later(t, "network", "delete", c.ref(br), "--project", "default")
		c.must(t, "init", c.image, c.ref(inst), "--project", lxdProject, "--storage", c.pool)
		c.later(t, "delete", c.ref(inst), "--project", lxdProject, "--force")
		c.must(t, "config", "device", "add", c.ref(inst), "pats0", "nic", "--project", lxdProject, "network="+br, "name=pats0")
		_, err := c.lxc("network", "acl", "delete", c.ref(acl), "--project", "default")
		assert.Error(t, err, "")
		_, err = c.lxc("network", "delete", c.ref(br), "--project", "default")
		assert.Error(t, err, "")
		c.must(t, "delete", c.ref(inst), "--project", lxdProject, "--force")
		c.must(t, "network", "delete", c.ref(br), "--project", "default")
		c.must(t, "network", "acl", "delete", c.ref(acl), "--project", "default")
	}},
	{"a published image's alias is taken once, and image info names its fingerprint", func(t *testing.T, c *contractEnv) {
		inst, alias := "pats-ct-"+c.tag+"-pub", "pats-img-ct-"+c.tag
		c.must(t, "init", c.image, c.ref(inst), "--project", lxdProject, "--storage", c.pool)
		c.later(t, "delete", c.ref(inst), "--project", lxdProject, "--force")
		c.must(t, "publish", c.ref(inst), c.remote+":", "--project", lxdProject, "--alias", alias, "user.pats.run=ct")
		c.later(t, "image", "delete", c.ref(alias), "--project", lxdProject)
		info := c.must(t, "image", "info", c.ref(alias), "--project", lxdProject)
		fp := ""
		for _, line := range strings.Split(info, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Fingerprint:"); ok {
				fp = strings.TrimSpace(v)
			}
		}
		require.That(t, len(fp) == 64, "no fingerprint line in lxc image info:\n%s", info)
		_, err := c.lxc("image", "alias", "create", c.ref(alias), fp, "--project", lxdProject)
		assert.Error(t, err, "")
		var imgs []struct {
			Fingerprint string            `json:"fingerprint"`
			Properties  map[string]string `json:"properties"`
		}
		require.NoError(t, json.Unmarshal([]byte(c.must(t, "query", c.remote+":/1.0/images?project="+lxdProject+"&recursion=1")), &imgs))
		found := false
		for _, im := range imgs {
			if im.Fingerprint == fp {
				found = true
				assert.Equal(t, im.Properties["user.pats.run"], "ct")
			}
		}
		assert.That(t, found, "the published image is not listed")
	}},
	{"a create names its instance in a running operation, and a delete meanwhile says not found", func(t *testing.T, c *contractEnv) {
		inst := "pats-ct-" + c.tag + "-op"
		c.slow(inst)
		var out bytes.Buffer
		x, err := c.r.Start(nil, &out, &out, "init", c.image, c.ref(inst), "--project", lxdProject, "--storage", c.pool)
		require.NoError(t, err)
		c.later(t, "delete", c.ref(inst), "--project", lxdProject, "--force")
		defer func() {
			_ = x.Signal(syscall.SIGKILL)
			_ = x.Wait()
		}()
		want := "/1.0/instances/" + inst + "?project=" + lxdProject
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var ops struct {
				Running []struct {
					Resources map[string][]string `json:"resources"`
				} `json:"running"`
			}
			require.NoError(t, json.Unmarshal([]byte(c.must(t, "query", c.remote+":/1.0/operations?project="+lxdProject+"&recursion=1")), &ops))
			for _, op := range ops.Running {
				for _, r := range op.Resources["instances"] {
					if r != want {
						continue
					}
					dout, derr := c.lxc("delete", c.ref(inst), "--project", lxdProject, "--force")
					if derr == nil {
						t.Skip("the create finished between the operation read and the delete")
					}
					assert.ContainsString(t, strings.ToLower(dout), "not found")
					return
				}
			}
			if _, err := c.lxc("query", c.remote+":/1.0/instances/"+inst+"?project="+lxdProject); err == nil {
				t.Skip("the create finished before its operation was seen")
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("no operation named the instance")
	}},
}

func contractTag() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestLXDContractFake(t *testing.T) {
	for _, sc := range lxdContracts {
		t.Run(sc.name, func(t *testing.T) {
			f := newFakeServer(t)
			f.projects[lxdProject] = true
			c := &contractEnv{r: runFunc(f.run), remote: f.remote, image: "img", pool: f.pool, tag: contractTag(),
				slow: func(name string) { f.mu.Lock(); f.slow[name] = 5; f.mu.Unlock() }}
			sc.run(t, c)
		})
	}
}

// TestLXDContract runs the scenarios against the cli's default remote. it
// needs PATS_LXD_E2E=1; PATS_LXD_E2E_IMAGE is what instances are made from
// (ubuntu:24.04).
func TestLXDContract(t *testing.T) {
	if os.Getenv("PATS_LXD_E2E") == "" {
		t.Skip("set PATS_LXD_E2E=1 to check the contracts against a real lxd server")
	}
	image := os.Getenv("PATS_LXD_E2E_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	// a process of its own sets the server up (the pats project, the pool)
	// as a run would.
	p := newProcess(wallClock{}, defaultTimings)
	defer p.shutdown()
	l := newLXD(p, lxcCLI, Options{})
	require.NoError(t, l.prepareServer(context.Background()))
	for _, sc := range lxdContracts {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, &contractEnv{r: lxcCLI, remote: l.o.Remote, image: image, pool: l.pool, tag: contractTag(), slow: func(string) {}})
		})
	}
}
