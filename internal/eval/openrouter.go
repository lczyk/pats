package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lczyk/pats/internal/config"
)

// the model check (docs/spec/model-preflight.md): the model of an agent whose
// harness names the openrouter catalogue is looked up in openrouter's public
// catalogue before any sandbox starts, so a typo'd, retired or tool-less model
// is a load error rather than a sandbox per pair.

// checkModels is the check Run makes; a var so tests that are not about it
// skip the network.
var checkModels = checkOpenrouterModels

// modelCheckBudget bounds the whole check.
const modelCheckBudget = 10 * time.Second

// openrouterURL is the api base the check reads; PATS_OPENROUTER_URL moves it
// (tests point it at a local server).
func openrouterURL() string {
	if u := os.Getenv("PATS_OPENROUTER_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://openrouter.ai/api/v1"
}

// routingSuffixes are openrouter's routing shortcuts: not catalogue ids, and
// taken in any order beside a catalogue variant (x/y:nitro:free is x/y:free
// routed by throughput).
var routingSuffixes = map[string]bool{"nitro": true, "floor": true, "online": true, "exacto": true}

// catalogueID is the catalogue id a model id names: its routing shortcuts
// dropped wherever they sit, the rest kept in order.
func catalogueID(id string) string {
	parts := strings.Split(id, ":")
	kept := parts[:1]
	for _, s := range parts[1:] {
		if !routingSuffixes[s] {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, ":")
}

type openrouterModel struct {
	ID                  string   `json:"id"`
	SupportedParameters []string `json:"supported_parameters"`
}

// checkOpenrouterModels checks each agent's model against the catalogue: it
// exists, and takes tools. the caller passes the agents whose harness names
// the catalogue (catalogueAgents). a model that fails is
// the error; a catalogue that cannot be read is a warning, and the run goes
// on. a cancelled ctx (ctrl-c) is never swallowed.
func checkOpenrouterModels(ctx context.Context, agents []config.Agent, client *http.Client) (warnings []string, err error) {
	check := agents
	if len(check) == 0 {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, modelCheckBudget)
	defer cancel()
	models, ferr := fetchCatalogue(cctx, client)
	if ferr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []string{"model check skipped: " + ferr.Error()}, nil
	}
	byID := map[string]openrouterModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	var errs []string
	for _, a := range check {
		id := catalogueID(a.Model)
		m, ok := byID[id]
		switch {
		case !ok:
			msg := fmt.Sprintf("agent %q: model %q is not on openrouter", a.Name, a.Model)
			if near := closestIDs(id, models, 3); len(near) > 0 {
				msg += "; closest: " + strings.Join(near, ", ")
			}
			errs = append(errs, msg)
		case !slices.Contains(m.SupportedParameters, "tools"):
			errs = append(errs, fmt.Sprintf("agent %q: model %q does not take tools, which opencode drives the agent with", a.Name, a.Model))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("model check:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil, nil
}

func fetchCatalogue(ctx context.Context, client *http.Client) ([]openrouterModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openrouterURL()+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter catalogue: %s", resp.Status)
	}
	var body struct {
		Data []openrouterModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("openrouter catalogue: %w", err)
	}
	if body.Data == nil {
		return nil, fmt.Errorf("openrouter catalogue: no data")
	}
	return body.Data, nil
}

// closestIDs is up to n catalogue ids nearest to id by edit distance.
func closestIDs(id string, models []openrouterModel, n int) []string {
	type cand struct {
		id string
		d  int
	}
	var cs []cand
	for _, m := range models {
		cs = append(cs, cand{m.ID, editDistance(id, m.ID)})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].id < cs[j].id
	})
	var out []string
	for _, c := range cs[:min(n, len(cs))] {
		out = append(out, c.id)
	}
	return out
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// catalogueAgents is the agents whose harness checks its models against
// catalogue.
func catalogueAgents(cfg *config.Config, agents []config.Agent, catalogue string) []config.Agent {
	var out []config.Agent
	for _, a := range agents {
		if hd := cfg.HarnessOf(a); hd != nil && hd.Models == catalogue {
			out = append(out, a)
		}
	}
	return out
}

// pairAgents is the distinct agents pairs run, in config order.
func pairAgents(cfg *config.Config, pairs []config.TestPair) []config.Agent {
	used := map[string]bool{}
	for _, p := range pairs {
		used[p.Agent] = true
	}
	var out []config.Agent
	for _, a := range cfg.Agents {
		if used[a.Name] {
			out = append(out, a)
		}
	}
	return out
}
