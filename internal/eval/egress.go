package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/lczyk/pats/internal/agent"
	"github.com/lczyk/pats/internal/config"
	"github.com/lczyk/pats/src/sandbox/proxy"
	"gopkg.in/yaml.v3"
)

// EffectivePolicy is the egress policy a pair runs under: the sandbox's, the
// task's lists unioned in, and -- with an agent -- the harness's own hosts.
// task and agent may be "". the sandbox must run the proxy.
func EffectivePolicy(cfg *config.Config, sandboxID, taskID, agentID string) (proxy.Policy, error) {
	var sb *config.Sandbox
	for i := range cfg.Sandboxes {
		if cfg.Sandboxes[i].Name == sandboxID {
			sb = &cfg.Sandboxes[i]
		}
	}
	if sb == nil {
		return proxy.Policy{}, fmt.Errorf("unknown sandbox %q", sandboxID)
	}
	if !sb.Egress.Set {
		return proxy.Policy{}, fmt.Errorf("sandbox %q runs no egress proxy (egress is %s)", sandboxID, sb.Egress.Mode())
	}
	var te config.TaskEgress
	if taskID != "" {
		found := false
		for _, t := range cfg.Tasks {
			if t.Name == taskID {
				te, found = t.Egress, true
			}
		}
		if !found {
			return proxy.Policy{}, fmt.Errorf("unknown task %q", taskID)
		}
	}
	harness := ""
	if agentID != "" {
		found := false
		for _, a := range cfg.Agents {
			if a.Name == agentID {
				harness, found = a.Harness, true
			}
		}
		if !found {
			return proxy.Policy{}, fmt.Errorf("unknown agent %q", agentID)
		}
	}
	return egressFor(*sb, te, harness, "", false).Policy, nil
}

// CheckURL prints the verdict a policy gives target -- a url, or a bare host
// taken as https -- and the entry that decided it. merged says whether the
// harness hosts are in the policy (an agent was named).
//
// behind the machine's proxy (chain non-nil) the verdict also says whether
// the url would go through it or direct (NO_PROXY).
func CheckURL(pol proxy.Policy, target, method string, merged bool, chain *proxy.Chain, out io.Writer) (bool, error) {
	r, err := pol.Rule()
	if err != nil {
		return false, err
	}
	if !strings.Contains(target, "://") {
		target = "https://" + target
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return false, fmt.Errorf("bad url %q", target)
	}
	port := uint16(443)
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return false, fmt.Errorf("bad port %q", p)
		}
		port = uint16(n)
	}
	if method == "" {
		method = "GET"
	}
	// the proxy matches the decoded path of the request line, which is "/"
	// for a bare host.
	path := u.Path
	if path == "" {
		path = "/"
	}

	var v proxy.Verdict
	how := "tunnel"
	switch {
	case u.Scheme == "http":
		how = "plain http"
		v = r.Decide(u.Hostname(), port, method, path, u.RawQuery)
	default:
		v = r.DecideHost(u.Hostname(), port)
		if v.Allowed && r.MitmHost(u.Hostname(), port) {
			how = "mitm"
			v = r.Decide(u.Hostname(), port, method, path, u.RawQuery)
		}
	}
	verdict := "deny "
	if v.Allowed {
		verdict = "allow"
	}
	notes := []string{how}
	if v.Reason != "" {
		notes = append(notes, "reason="+v.Reason)
	}
	if pol.Default == "allow" {
		notes = append(notes, "default: allow")
	} else {
		notes = append(notes, "default: deny")
	}
	if len(pol.Methods) > 0 {
		m := "methods " + strings.Join(pol.Methods, ",") + " inspect every allowed host but the harness's"
		if pol.Default == "allow" {
			m = "methods " + strings.Join(pol.Methods, ",") + " under default: allow inspect every host reached but the harness's"
		}
		notes = append(notes, m)
	}
	if chain != nil {
		scheme := "https"
		if u.Scheme == "http" {
			scheme = "http"
		}
		if up := chain.For(scheme, net.JoinHostPort(u.Hostname(), strconv.Itoa(int(port)))); up != nil {
			notes = append(notes, "via upstream "+proxy.Redact(up))
		} else {
			notes = append(notes, "direct, not via the upstream")
		}
	}
	if !merged {
		notes = append(notes, "sandbox and task only, harness hosts not merged (pass --agent)")
	}
	fmt.Fprintf(out, "%s rule=%s  (%s)\n", verdict, v.Rule, strings.Join(notes, "; "))
	return v.Allowed, nil
}

// EgressLog prints a pair's egress.log as a table. denied keeps only
// refusals; methods keeps only requests with one of those methods.
func EgressLog(path string, denied bool, methods []string, out io.Writer) error {
	evs, err := readEvents(path)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, m := range methods {
		for _, x := range strings.Split(m, ",") {
			if x = strings.ToUpper(strings.TrimSpace(x)); x != "" {
				want[x] = true
			}
		}
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tEV\tKIND\tVERDICT\tTARGET\tSTATUS\tBYTES\tRULE")
	for _, e := range evs {
		if denied && (e.Allowed || e.Ev == "close") {
			continue
		}
		if len(want) > 0 && (e.Ev != "req" || !want[e.Method]) {
			continue
		}
		verdict := "allow"
		if !e.Allowed {
			verdict = "deny"
		}
		if e.Ev == "close" {
			verdict = "-"
		}
		target := e.Host
		if e.Port != "" && e.Port != "80" && e.Port != "443" {
			target += ":" + e.Port
		}
		if e.URL != "" {
			target = e.URL
		}
		if e.Method != "" {
			target = e.Method + " " + target
		}
		rule := e.Rule
		if e.Reason != "" {
			rule += " (" + e.Reason + ")"
		}
		status := "-"
		if e.Status != 0 {
			status = strconv.Itoa(e.Status)
		}
		ts := e.TS
		if i := strings.IndexByte(ts, 'T'); i >= 0 {
			ts = strings.TrimSuffix(ts[i+1:], "Z")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ts, e.Ev, e.Kind, verdict, target, status,
			humanBytes(e.In+e.Out), rule)
	}
	return tw.Flush()
}

func readEvents(path string) ([]proxy.Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []proxy.Event
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e proxy.Event
		if json.Unmarshal(line, &e) == nil && e.Ev != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// SuggestEgress folds every egress.log of a run into an allow block: the hosts
// the agents reached, or tried to and nobody had anticipated (a default
// refusal). each pair's own harness hosts are left out (pats merges them per
// agent), and so are hosts the policy names in a deny. request lines are grouped by user
// agent; tunnels show neither user agent nor method, so they are listed on
// their own.
func SuggestEgress(configDir, runArg string, out io.Writer) error {
	runDir, err := resolveRunDir(filepath.Join(configDir, runsSubdir), runArg)
	if err != nil {
		return err
	}
	logs, _ := filepath.Glob(filepath.Join(runDir, "*", "*", "egress.log"))
	if len(logs) == 0 {
		return errors.New("no egress.log in " + runDir + " (no pair ran under an egress proxy)")
	}

	byUA := map[string]map[string]bool{} // ua -> hosts
	tunnels := map[string]bool{}
	harnessSeen := map[string]bool{}
	readsOnly := true
	for _, l := range logs {
		evs, err := readEvents(l)
		if err != nil {
			return err
		}
		harness, err := harnessRule(pairHarness(filepath.Dir(l)))
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.Ev == "close" || (!e.Allowed && e.Rule != "default") {
				continue
			}
			port, _ := strconv.ParseUint(e.Port, 10, 16)
			host := e.Host
			if port != 0 && port != 80 && port != 443 {
				host += ":" + e.Port
			}
			if harness.DecideHost(e.Host, uint16(port)).Allowed {
				harnessSeen[host] = true
				continue
			}
			switch e.Ev {
			case "open":
				if e.Kind != "mitm" {
					tunnels[host] = true
					readsOnly = false
				}
			case "req":
				if e.Method != "GET" && e.Method != "HEAD" {
					readsOnly = false
				}
				ua := e.UA
				if ua == "" {
					ua = "(no user agent)"
				}
				if byUA[ua] == nil {
					byUA[ua] = map[string]bool{}
				}
				byUA[ua][host] = true
			}
		}
	}

	fmt.Fprintf(out, "# from %s -- review before use: everything here was reached, not vetted.\n", relToCwd(runDir))
	fmt.Fprintln(out, "egress:")
	fmt.Fprintln(out, "  allow:")
	// the entries as yaml writes a list of strings: a host is not always a
	// plain scalar, and the quoting is the encoder's to know.
	entries := func(hosts []string) {
		if len(hosts) == 0 {
			return
		}
		b, _ := yaml.Marshal(hosts)
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			fmt.Fprintf(out, "    %s\n", line)
		}
	}
	if len(tunnels) > 0 {
		fmt.Fprintln(out, "    # tunnels (no user agent or method visible)")
		entries(sortedSet(tunnels))
	}
	uas := make([]string, 0, len(byUA))
	for ua := range byUA {
		uas = append(uas, ua)
	}
	sort.Strings(uas)
	for _, ua := range uas {
		// a comment is one line: a line break of any kind in a user agent,
		// which the agent chose, would end it.
		fmt.Fprintf(out, "    # %s\n", strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || r == '\u0085' {
				return ' '
			}
			return r
		}, ua))
		entries(slices.DeleteFunc(sortedSet(byUA[ua]), func(h string) bool { return tunnels[h] }))
	}
	switch {
	case len(tunnels) > 0:
		fmt.Fprintln(out, "  # methods: unknown -- tunnels hide them")
	case len(byUA) > 0 && readsOnly:
		fmt.Fprintln(out, "  # only GET and HEAD were seen: refuse writes everywhere (every allowed host gets inspected)")
		fmt.Fprintln(out, "  methods: [GET, HEAD]")
	}
	if len(harnessSeen) > 0 {
		fmt.Fprintf(out, "  # harness hosts, merged per agent by pats: %s\n", strings.Join(sortedSet(harnessSeen), ", "))
	}
	return nil
}

// harnessRule matches the hosts a harness needs, for leaving them out of a
// suggestion. an unknown harness (a run from before metadata named it) gets
// every harness's hosts.
func harnessRule(harness string) (proxy.Rule, error) {
	hosts, ok := agent.RequiredHosts[harness]
	if !ok {
		for _, hs := range agent.RequiredHosts {
			hosts = append(hosts, hs...)
		}
	}
	return proxy.ParseRule(false, hosts, nil)
}

// pairHarness reads the harness a pair ran from its metadata, "" if unknown.
func pairHarness(pairDir string) string {
	var pm pairMeta
	if b, err := os.ReadFile(filepath.Join(pairDir, "metadata.json")); err == nil && json.Unmarshal(b, &pm) == nil {
		return pm.Harness
	}
	return ""
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// humanBytes renders a byte count with a binary suffix: 512, 1.5k, 3.2M.
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprint(n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fk", float64(n)/1024)
	case n < 1<<30:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}
