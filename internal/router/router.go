// Package router holds the route snapshot and picks provider candidates.
package router

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2" // nosemgrep: math-random-used, go.lang.security.audit.crypto.math_random.math-random-used -- 只做加权选路，非安全用途
	"sort"
	"sync/atomic"
	"time"

	"github.com/aws-samples/sample-llm-gateway/internal/controlplane"
)

// Candidate is a resolved provider attempt for a model.
type Candidate struct {
	ProviderCode      string
	ProviderModelCode string
	Priority          int
	Weight            int
}

type snapshot struct {
	version string
	// tiers per model: ascending priority value (lower number = tried first), each tier sorted by weight desc
	models map[string][][]Candidate
}

// Router serves route lookups from an atomically swapped snapshot.
// Weighted picks use the package-level math/rand/v2 source, which is safe for concurrent
// use; a per-Router *rand.Rand is not, and Attempts runs on every request goroutine.
type Router struct {
	snap atomic.Pointer[snapshot]
}

func New() *Router { return &Router{} }

// Load replaces the current snapshot.
func (r *Router) Load(routes *controlplane.Routes) {
	s := &snapshot{version: routes.Version, models: make(map[string][][]Candidate, len(routes.Models))}
	for _, m := range routes.Models {
		byPrio := map[int][]Candidate{}
		for _, p := range m.Providers {
			w := p.Weight
			if w <= 0 {
				w = 1
			}
			byPrio[p.Priority] = append(byPrio[p.Priority], Candidate{
				ProviderCode: p.ProviderCode, ProviderModelCode: p.ProviderModelCode, Priority: p.Priority, Weight: w,
			})
		}
		prios := make([]int, 0, len(byPrio))
		for k := range byPrio {
			prios = append(prios, k)
		}
		sort.Ints(prios)
		tiers := make([][]Candidate, 0, len(prios))
		for _, k := range prios {
			tiers = append(tiers, byPrio[k])
		}
		s.models[m.ModelCode] = tiers
	}
	r.snap.Store(s)
}

// Ready reports whether a snapshot has been loaded.
func (r *Router) Ready() bool { return r.snap.Load() != nil }

// Version returns the current route version (ETag).
func (r *Router) Version() string {
	if s := r.snap.Load(); s != nil {
		return s.version
	}
	return ""
}

// Models lists user-visible model codes in the snapshot.
func (r *Router) Models() []string {
	s := r.snap.Load()
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.models))
	for k := range s.models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Attempts returns the ordered list of candidates to try for a model:
// within each priority tier a weighted random order, tiers in ascending priority.
// Returns nil if the model is unknown.
func (r *Router) Attempts(model string) []Candidate {
	s := r.snap.Load()
	if s == nil {
		return nil
	}
	tiers, ok := s.models[model]
	if !ok {
		return nil
	}
	var out []Candidate
	for _, tier := range tiers {
		out = append(out, r.weightedOrder(tier)...)
	}
	return out
}

// weightedOrder produces a permutation where higher weight is more likely to come first.
func (r *Router) weightedOrder(tier []Candidate) []Candidate {
	if len(tier) == 1 {
		return []Candidate{tier[0]}
	}
	rem := make([]Candidate, len(tier))
	copy(rem, tier)
	out := make([]Candidate, 0, len(tier))
	for len(rem) > 0 {
		total := 0
		for _, c := range rem {
			total += c.Weight
		}
		pick := rand.IntN(total)
		idx := 0
		for i, c := range rem {
			pick -= c.Weight
			if pick < 0 {
				idx = i
				break
			}
		}
		out = append(out, rem[idx])
		rem = append(rem[:idx], rem[idx+1:]...)
	}
	return out
}

// Poller keeps the router fresh from the control plane using ETag polling.
type Poller struct {
	cp       *controlplane.Client
	router   *Router
	interval time.Duration
	etag     string
	log      *slog.Logger
	// Rejected is incremented each time a snapshot is refused (see ErrEmptyRoutes). Optional.
	Rejected interface{ Inc() }
}

func NewPoller(cp *controlplane.Client, r *Router, interval time.Duration, log *slog.Logger) *Poller {
	return &Poller{cp: cp, router: r, interval: interval, log: log}
}

// ErrEmptyRoutes is returned by Sync when the control plane sends a table with zero models
// while the router already holds a non-empty snapshot. A control-plane bug must not be able
// to blank out routing; the last good snapshot stays and the ETag is not advanced so the next
// poll fetches the full table again.
var ErrEmptyRoutes = errors.New("empty route table rejected, keeping last snapshot")

// Sync fetches once. Returns nil on 304.
func (p *Poller) Sync(ctx context.Context) error {
	routes, etag, err := p.cp.FetchRoutes(ctx, p.etag)
	if err != nil {
		if err == controlplane.ErrNotModified {
			return nil
		}
		return err
	}
	if len(routes.Models) == 0 && len(p.router.Models()) > 0 {
		if p.Rejected != nil {
			p.Rejected.Inc()
		}
		return ErrEmptyRoutes
	}
	p.router.Load(routes)
	p.etag = etag
	p.log.Info("routes loaded", "version", routes.Version, "models", len(routes.Models))
	return nil
}

// Run polls until ctx is done. Failures keep the last snapshot.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := p.Sync(cctx); err != nil {
				p.log.Warn("routes poll failed, keeping last snapshot", "err", err)
			}
			cancel()
		}
	}
}
