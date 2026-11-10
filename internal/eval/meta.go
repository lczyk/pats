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

	"github.com/lczyk/pats/internal/config"
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
}

// advisoryEgress says whether the pair's egress policy was advisory: what
// went around the proxy left no audit line, so an empty denied list there
// is not clean.
func (m *pairMeta) advisoryEgress() bool {
	return m != nil && m.Egress != nil && m.Egress.Enforced != nil && !*m.Egress.Enforced
}

type pairMeta struct {
	Agent   string `json:"agent"`
	Harness string `json:"harness,omitempty"`
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
	LXD       *lxdProvenance `json:"lxd,omitempty"`
	ExitCode  int            `json:"exit_code"`
	DurationS float64        `json:"duration_s"`
	Status    string         `json:"status"` // ok | nonzero | error
	Error     string         `json:"error,omitempty"`
	// Synced is whether the agent's files came home, so collect saw them; a
	// pair from before the field has nil (see filesHome).
	Synced      *bool  `json:"synced"`
	PatsVersion string `json:"pats_version"`
	// egress the proxy refused, by kind: denied is what no rule anticipated
	// (the cheat signal), blocked is what the policy names, failed is upstream
	// trouble. see readEgressAudit.
	DeniedEgress  []string      `json:"denied_egress,omitempty"`
	BlockedEgress []string      `json:"blocked_egress,omitempty"`
	FailedEgress  []string      `json:"failed_egress,omitempty"`
	Egress        *egressRecord `json:"egress,omitempty"`
	// per-harness digest parsed from the log: cost, tokens, turns, tools.
	Summary *runSummary `json:"summary,omitempty"`
	// the harness versions inside the image, as pats init stamped them.
	Components map[string]string `json:"components,omitempty"`
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
	Denied   []string // rule: default -- nobody anticipated it; the cheat signal
	Blocked  []string // a policy refusal the agent triggered
	Failed   []string // upstream trouble: revoked certificate, dial failure, the machine's proxy refusing
	Requests int      // open and req lines
	In, Out  int64    // bytes from and to upstream
}

// failedReasons are refusals the agent did not cause.
var failedReasons = map[string]bool{"revoked": true, "dial": true}

// readEgressAudit folds an audit log; a missing file is the zero value (no
// proxy ran). lines that are not json (there should be none) are skipped.
func readEgressAudit(auditPath string) egressAudit {
	var a egressAudit
	data, err := os.ReadFile(auditPath)
	if err != nil {
		return a
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
	return a
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
