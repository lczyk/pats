package eval

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/assert/require"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/nestedprobe"
	"github.com/lczyk/pats/src/sandbox"
)

// the metadata's nested object: the probe's verdicts and ok for vms (null
// when it did not run), the request alone for containers; it reads back
// for the report.
func TestNestedRecord(t *testing.T) {
	enc := func(r *sandbox.NestedResult) string {
		b, err := json.Marshal(nestedRecordOf(r))
		require.NoError(t, err)
		return string(b)
	}
	ok := &sandbox.NestedResult{Asked: "vms", Required: true, Probed: true,
		Probe: nestedprobe.Result{KVM: "ok", VhostVsockCID: "ok", Vsock: "ok", VhostNet: "missing"}}
	assert.Equal(t, enc(ok), `{"asked":"vms","required":true,"ok":true,"kvm":"ok","vhost_vsock_cid":"ok","vsock":"ok","vhost_net":"missing"}`)
	lost := &sandbox.NestedResult{Asked: "vms", Reason: "the probe binary: fetch failed"}
	assert.Equal(t, enc(lost), `{"asked":"vms","required":false,"ok":null,"kvm":"unknown","vhost_vsock_cid":"unknown","vsock":"unknown","vhost_net":"unknown","reason":"the probe binary: fetch failed"}`)
	assert.Equal(t, enc(&sandbox.NestedResult{Asked: "containers", Set: "security.nesting"}), `{"asked":"containers","set":"security.nesting"}`)
	assert.Equal(t, enc(&sandbox.NestedResult{Asked: "containers"}), `{"asked":"containers","set":null}`)
	assert.That(t, nestedRecordOf(nil) == nil, "a record for nothing asked")
	noNet := *ok
	noNet.Reason = "vhost-net: no /dev/vhost-net"
	assert.Equal(t, nestedRecordOf(&noNet).Reason, "")

	for _, tc := range []struct {
		r     *sandbox.NestedResult
		short bool
	}{{ok, false}, {lost, true}, {&sandbox.NestedResult{Asked: "containers"}, false}} {
		var m pairMeta
		require.NoError(t, json.Unmarshal([]byte(`{"nested":`+enc(tc.r)+`}`), &m))
		assert.Equal(t, m.nestedShort(), tc.short, enc(tc.r))
	}
	assert.That(t, !(&pairMeta{}).nestedShort(), "short without a record")
}

// a raw security.nesting beside nested on a container is redundant: said
// once before the pairs, and not on a sandbox no pair uses.
func TestWarnNestedRawKey(t *testing.T) {
	cfg := parseCfg(t, `boxes: {kind: sandbox, items: {lxx: {driver: lxd, nested: containers, config: {security.nesting: "true"}}, lyy: {driver: lxd, nested: containers, config: {security.nesting: "true"}}}}
agents: {kind: agent, items: {lax: {harness: opencode-openrouter, model: m, sandbox: lxx}, lay: {harness: opencode-openrouter, model: m, sandbox: lyy}}}
tasks: {kind: task, items: {txx: {prompt: p}}}
suites: {kind: suite, items: {su2: {agents: [lax, lay], tasks: txx}}}
`)
	var out bytes.Buffer
	warnUnboxed(cfg, []config.TestPair{{Agent: "lax", Task: "txx"}}, logw{w: &out})
	assert.ContainsString(t, out.String(), `sandbox "lxx": nested: containers sets security.nesting itself`)
	assert.That(t, !strings.Contains(out.String(), "lyy"), "warned of an unused sandbox: %s", out.String())
}
