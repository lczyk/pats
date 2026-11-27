package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/internal/nestedprobe"
	"github.com/lczyk/pats/src/sandbox"
	"github.com/lczyk/pats/src/sandbox/proxy"
)

// egressRecord is what a pair ran under and what crossed the proxy, so a run
// is reproducible from its own output.
type egressRecord struct {
	Policy   proxy.Policy `json:"policy"`
	Image    string       `json:"image,omitempty"` // the sidecar image id (docker / podman)
	Requests int          `json:"requests"`
	BytesIn  int64        `json:"bytes_in"`
	BytesOut int64        `json:"bytes_out"`
	// Enforced is whether nothing but the proxy could reach out: false under
	// the host driver, where egress is advisory. nil on a pair from before.
	Enforced *bool `json:"enforced,omitempty"`
	// KeysSubstituted counts the requests a key proxy put a real key in.
	KeysSubstituted int `json:"keys_substituted,omitempty"`
}

// nestedRecord is a pair's nested object: for vms, the probe's verdicts
// with ok true, false or null (the probe did not run); for containers, the
// request and what pats set for it, nothing checked.
type nestedRecord struct {
	Asked         string  `json:"asked"`
	Required      bool    `json:"required"`
	Set           *string `json:"set"`
	OK            *bool   `json:"ok"`
	KVM           string  `json:"kvm,omitempty"`
	VhostVsockCID string  `json:"vhost_vsock_cid,omitempty"`
	Vsock         string  `json:"vsock,omitempty"`
	VhostNet      string  `json:"vhost_net,omitempty"`
	Reason        string  `json:"reason,omitempty"`
}

// MarshalJSON writes the shape of the record's kind.
func (n nestedRecord) MarshalJSON() ([]byte, error) {
	if n.Asked == sandbox.NestedContainers {
		return json.Marshal(struct {
			Asked string  `json:"asked"`
			Set   *string `json:"set"`
		}{n.Asked, n.Set})
	}
	type plain nestedRecord
	p := plain(n)
	p.Set = nil
	return json.Marshal(struct {
		plain
		Set *string `json:"set,omitempty"`
	}{plain: p})
}

func nestedRecordOf(r *sandbox.NestedResult) *nestedRecord {
	if r == nil {
		return nil
	}
	n := &nestedRecord{Asked: r.Asked, Required: r.Required}
	if r.Asked == sandbox.NestedContainers {
		if r.Set != "" {
			s := r.Set
			n.Set = &s
		}
		return n
	}
	p := r.Probe
	if !r.Probed {
		p = nestedprobe.Unknowns()
	}
	n.OK = r.OK()
	n.KVM, n.VhostVsockCID, n.Vsock, n.VhostNet = p.KVM, p.VhostVsockCID, p.Vsock, p.VhostNet
	// an optional check's failure (vhost-net) is in the verdicts, not a
	// reason beside ok: true.
	if n.OK == nil || !*n.OK {
		n.Reason = r.Reason
	}
	return n
}

// nestedShort says whether the pair ran without the nested vms it asked
// for: required: false, and the probe failed or did not run.
func (m *pairMeta) nestedShort() bool {
	return m != nil && m.Nested != nil && m.Nested.Asked == sandbox.NestedVMs && (m.Nested.OK == nil || !*m.Nested.OK)
}

// advisoryEgress says whether the pair's egress policy was advisory: what
// went around the proxy left no audit line, so an empty denied list there
// is not clean.
func (m *pairMeta) advisoryEgress() bool {
	return m != nil && m.Egress != nil && m.Egress.Enforced != nil && !*m.Egress.Enforced
}

// stepsRecord is where a pair's or a scorer's steps ran: the step sandbox
// and the pinned image, when not on the host.
type stepsRecord struct {
	Sandbox string `json:"sandbox"`
	Image   string `json:"image,omitempty"`
}

// pairTimings is where a pair's wall time went beside the agent's own run
// (duration_s), in seconds. a step that did not run, or took under a
// hundredth of a second, is left out.
type pairTimings struct {
	StageS   float64 `json:"stage_s,omitempty"`   // stage and prompt-cmd, with the step sandbox they ran in
	StartS   float64 `json:"start_s,omitempty"`   // the sandbox coming up, and its handover to the agent
	PrepareS float64 `json:"prepare_s,omitempty"` // the in-sandbox prepare
	SyncS    float64 `json:"sync_s,omitempty"`    // the agent's files coming home
	CloseS   float64 `json:"close_s,omitempty"`   // the sandbox and its proxy going away
	CollectS float64 `json:"collect_s,omitempty"` // collect, with its step sandbox
	GateS    float64 `json:"gate_s,omitempty"`    // the wait at the preflight gate, between setup and the agent
	TotalS   float64 `json:"total_s"`             // the pair up to this record: the agent's run included, the workdir's removal not
}

// lap times one step into d: the func it returns ends the step. a step
// timed twice adds up.
func lap(d *float64) func() {
	t0 := time.Now()
	return func() { *d += time.Since(t0).Seconds() }
}

// rounded is t as metadata.json records it.
func (t pairTimings) rounded() *pairTimings {
	for _, f := range []*float64{&t.StageS, &t.StartS, &t.PrepareS, &t.SyncS, &t.CloseS, &t.CollectS, &t.GateS, &t.TotalS} {
		*f = round2(*f)
	}
	return &t
}

type pairMeta struct {
	Agent   string `json:"agent"`
	Harness string `json:"harness,omitempty"`
	// where the harness's definition is from (default, machine or project),
	// and a digest of it: the name alone can mean another definition on
	// another machine or pats release.
	HarnessFrom string `json:"harness_from,omitempty"`
	HarnessHash string `json:"harness_hash,omitempty"`
	// the model and effort the agent ran with: the agent id alone can be
	// pointed at another model between runs (the dry run's estimate matches
	// on them).
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Task    string `json:"task"`
	Sandbox string `json:"sandbox"`
	// Driver ran the pair; SandNotBoxed says it ran on the machine itself
	// (the host driver), with no isolation.
	Driver       string `json:"driver,omitempty"`
	SandNotBoxed bool   `json:"sand_not_boxed,omitempty"`
	Image        string `json:"image"`
	Sudo         bool   `json:"sudo,omitempty"` // the agent could sudo (lxd)
	// the lxd server and image the pair ran on.
	LXD *lxdProvenance `json:"lxd,omitempty"`
	// Steps is where the task's host steps ran, absent on the host.
	Steps     *stepsRecord `json:"steps,omitempty"`
	ExitCode  int          `json:"exit_code"`
	DurationS float64      `json:"duration_s"`
	Timings   *pairTimings `json:"timings,omitempty"` // nil on a pair from before
	Status    string       `json:"status"`            // ok | nonzero | error
	Error     string       `json:"error,omitempty"`
	// BudgetKind is the cap of the agent's budget that stopped the pair:
	// cost, tokens, tools or time; "" when none did.
	BudgetKind string `json:"budget_kind,omitempty"`
	// Synced is whether the agent's files came home, so collect saw them; a
	// pair from before the field has nil (see filesHome).
	Synced      *bool  `json:"synced"`
	PatsVersion string `json:"pats_version"`
	// egress the proxy refused, by kind: denied is what no rule anticipated
	// (the cheat signal), blocked is what the policy names, failed is upstream
	// trouble. see readEgressAudit.
	DeniedEgress  []string `json:"denied_egress,omitempty"`
	BlockedEgress []string `json:"blocked_egress,omitempty"`
	FailedEgress  []string `json:"failed_egress,omitempty"`
	// KeyMisuse are the urls a placeholder was refused at, off its binding
	// (docs/spec/key-proxy.md): the agent tried to spend a held key elsewhere.
	KeyMisuse []string      `json:"key_misuse,omitempty"`
	Egress    *egressRecord `json:"egress,omitempty"`
	// Nested is what the sandbox's nested declaration found, absent when
	// it made none (docs/spec/nested.md).
	Nested *nestedRecord `json:"nested,omitempty"`
	// per-harness digest parsed from the log: cost, tokens, turns, tools.
	Summary *runSummary `json:"summary,omitempty"`
	// the harness versions inside the image, as pats init stamped them.
	Components map[string]string `json:"components,omitempty"`
	// what init placed of each harness cli in the image: its version, its
	// artefact's hash and its tree's id.
	Placed map[string]map[string]string `json:"placed,omitempty"`
	// a judge's sample: the pair it graded, and which sample it is.
	Judged *judged `json:"judged,omitempty"`
	Sample int     `json:"sample,omitempty"`
}

// filesHome says whether the pair's files came home. a pair written before
// the synced field counts as home when it ended ok or nonzero.
func (m *pairMeta) filesHome() bool {
	if m.Synced != nil {
		return *m.Synced
	}
	return m.Status == "ok" || m.Status == "nonzero"
}

// egressAudit is a pair's egress.log folded into what the run reports: the
// three refusal lists, each a unique target (the url when the proxy saw one,
// else the host), and the traffic totals.
type egressAudit struct {
	Denied      []string // rule: default -- nobody anticipated it; the cheat signal
	Blocked     []string // a policy refusal the agent triggered
	Failed      []string // upstream trouble: revoked certificate, dial failure, the machine's proxy refusing
	KeyMisuse   []string // a held key's placeholder off its binding
	Substituted int      // requests a held key went out in
	Requests    int      // open and req lines
	In, Out     int64    // bytes from and to upstream
}

// failedReasons are refusals the agent did not cause: key-scrub is an answer
// the key proxy could not read to scrub.
var failedReasons = map[string]bool{"revoked": true, "dial": true, "key-scrub": true}

// readEgressAudit folds an audit log; a missing file is the zero value (no
// proxy ran). lines that are not json (there should be none) are skipped.
func readEgressAudit(auditPath string) egressAudit {
	a, _ := readEgressAuditErr(auditPath)
	return a
}

// readEgressAuditErr is readEgressAudit with the error reading the file.
func readEgressAuditErr(auditPath string) (egressAudit, error) {
	var a egressAudit
	data, err := os.ReadFile(auditPath)
	if err != nil {
		return a, err
	}
	seen := map[string]bool{}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e proxy.Event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Ev {
		case "open", "req":
			a.Requests++
		case "close":
			a.In += e.In
			a.Out += e.Out
			continue
		default:
			continue
		}
		if e.Ev == "req" {
			a.In += e.In
			a.Out += e.Out
			if e.Key == "substituted" {
				a.Substituted++
			}
		}
		if e.Allowed {
			continue
		}
		target := e.Host
		if e.URL != "" {
			target = e.URL
		}
		var list *[]string
		switch {
		case e.Rule == "key" && e.Reason == "misuse":
			list = &a.KeyMisuse
		case failedReasons[e.Reason], strings.HasPrefix(e.Reason, "upstream: "):
			list = &a.Failed
		case e.Rule == "default" && e.Reason == "":
			list = &a.Denied
		default:
			list = &a.Blocked
		}
		key := fmt.Sprintf("%p\x00%s", list, target)
		if !seen[key] {
			seen[key] = true
			*list = append(*list, target)
		}
	}
	return a, nil
}

// lxdProvenance is what a pair's metadata records of the lxd server and
// image it ran on.
type lxdProvenance struct {
	Remote           string `json:"remote"`
	InstanceType     string `json:"instance_type"` // container | virtual-machine
	ImageFingerprint string `json:"image_fingerprint"`
	ServerVersion    string `json:"server_version"`
}

// lxdProvenances caches each lxd sandbox's provenance for the run: one
// server round trip per sandbox, not per pair.
var lxdProvenances sync.Map

func lxdProvenanceOf(ctx context.Context, sb config.Sandbox) *lxdProvenance {
	if v, ok := lxdProvenances.Load(sb.Name + "\x00" + sb.Image); ok {
		return v.(*lxdProvenance)
	}
	h, err := hostFor(sb)
	if err != nil {
		return nil
	}
	t, err := h.Target(ctx)
	if err != nil {
		return nil
	}
	p := &lxdProvenance{Remote: strings.TrimPrefix(t.Key, "lxd:"), InstanceType: "container", ServerVersion: t.Version}
	if sb.VM {
		p.InstanceType = "virtual-machine"
	}
	if img, err := h.Resolve(ctx, sb.Image); err == nil {
		p.ImageFingerprint = img.ID
	}
	lxdProvenances.Store(sb.Name+"\x00"+sb.Image, p)
	return p
}

// writeJSON replaces path through a temp file and a rename, so a write cut
// short leaves the old file or none, never half of one.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// maskedPolicy is a pair's policy as its metadata records it: a held key's
// placeholder is the pair's own and worth nothing, but it is no record.
func maskedPolicy(p proxy.Policy) proxy.Policy {
	if len(p.Keys) == 0 {
		return p
	}
	keys := make([]proxy.KeyBinding, len(p.Keys))
	for i, k := range p.Keys {
		keys[i] = proxy.KeyBinding{Name: k.Name, Placeholder: "<placeholder>", Bind: k.Bind}
	}
	p.Keys = keys
	return p
}
