package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

const (
	// resultTTL is how long one account's successful result is reused, even
	// by ?refresh=1, so repeated refreshes cannot hammer provider APIs.
	resultTTL = 5 * time.Minute
	// errorTTL bounds retries of a failed account.
	errorTTL = time.Minute
	// profileTTL caches Claude account identities by token fingerprint.
	profileTTL     = time.Hour
	sessionTTL     = 30 * time.Second
	refreshTimeout = 45 * time.Second
	requestTimeout = 15 * time.Second
	maxParallel    = 4
)

// Config wires the service. Zero values select production readers; tests
// inject fakes so no real credentials, keychain, or logs are touched.
type Config struct {
	Local     LocalCredentials
	Sessions  sessionReader
	UserAgent string
	Now       func() time.Time
}

type cachedResult struct {
	at      time.Time
	status  string
	code    string
	plan    string
	windows []Window
}

type profileEntry struct {
	at      time.Time
	profile claudeProfile
}

type refreshCall struct {
	done   chan struct{}
	cancel context.CancelFunc
	status int
	body   []byte
}

// Service builds quota reports. Plain snapshots never touch the network;
// refreshes query provider APIs through the CPA host and share one in-flight
// run.
type Service struct {
	mu        sync.Mutex
	host      HostFunc
	local     LocalCredentials
	sessions  sessionReader
	userAgent string
	now       func() time.Time

	results     map[string]cachedResult
	profiles    map[string]profileEntry
	last        []Account
	poolStatus  string
	refreshedAt time.Time
	session     *sessionObservation
	sessionAt   time.Time
	inflight    *refreshCall
	paused      bool
	closed      bool
	baseCtx     context.Context
	cancelBase  context.CancelFunc
}

func New(cfg Config) *Service {
	if cfg.Local == nil || cfg.Sessions == nil {
		creds := newFileCredentials()
		if cfg.Local == nil {
			cfg.Local = creds
		}
		if cfg.Sessions == nil {
			cfg.Sessions = newSessionReader(creds)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "cpa-harness-usage"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		local:      cfg.Local,
		sessions:   cfg.Sessions,
		userAgent:  cfg.UserAgent,
		now:        cfg.Now,
		results:    map[string]cachedResult{},
		profiles:   map[string]profileEntry{},
		poolStatus: PoolUnavailable,
		baseCtx:    ctx,
		cancelBase: cancel,
	}
}

// SetHost installs the CPA host callback. Without it only local Codex
// session logs are reported.
func (s *Service) SetHost(host HostFunc) {
	s.mu.Lock()
	s.host = host
	s.mu.Unlock()
}

// Pause cancels a running refresh and drops cached results.
func (s *Service) Pause() {
	s.mu.Lock()
	s.paused = true
	s.resetLocked()
	s.mu.Unlock()
}

func (s *Service) Resume() {
	s.mu.Lock()
	if !s.closed {
		s.paused = false
	}
	s.mu.Unlock()
}

// Close stops the service permanently. It never waits for a refresh.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.paused = true
	s.resetLocked()
	s.cancelBase()
	s.host = nil
	s.mu.Unlock()
}

func (s *Service) resetLocked() {
	s.results = map[string]cachedResult{}
	s.profiles = map[string]profileEntry{}
	s.last = nil
	s.refreshedAt = time.Time{}
	s.session = nil
	s.sessionAt = time.Time{}
	s.poolStatus = PoolUnavailable
	if s.inflight != nil {
		s.inflight.cancel()
		s.inflight = nil
	}
}

var unavailableBody = []byte(`{"error":"unavailable"}`)

// Snapshot returns an HTTP status and JSON body. Without refresh it reports
// the last refresh plus the newest local Codex session log, offline.
func (s *Service) Snapshot(refresh bool) (int, []byte) {
	s.mu.Lock()
	if s.closed || s.paused {
		s.mu.Unlock()
		return 503, unavailableBody
	}
	if !refresh {
		s.mu.Unlock()
		return s.encode(false)
	}
	if call := s.inflight; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.status, call.body
		case <-s.baseCtx.Done():
			return 503, unavailableBody
		}
	}
	ctx, cancel := context.WithTimeout(s.baseCtx, refreshTimeout)
	call := &refreshCall{done: make(chan struct{}), cancel: cancel}
	s.inflight = call
	host := s.host
	s.mu.Unlock()

	accounts, poolStatus := s.refresh(ctx, host)
	canceled := ctx.Err() == context.Canceled
	cancel()

	s.mu.Lock()
	current := s.inflight == call
	if current {
		s.inflight = nil
	}
	if current && !canceled && !s.paused && !s.closed {
		s.last = accounts
		s.poolStatus = poolStatus
		s.refreshedAt = s.now()
	}
	s.mu.Unlock()

	status, body := 503, unavailableBody
	if current && !canceled {
		status, body = s.encode(true)
	}
	call.status, call.body = status, body
	close(call.done)
	return status, body
}

// encode serializes the last accounts merged with the session-log fallback.
// live keeps SourceLive on results fetched by the current refresh.
func (s *Service) encode(live bool) (int, []byte) {
	session, hasSession := s.sessionObservation()
	s.mu.Lock()
	if s.closed || s.paused {
		s.mu.Unlock()
		return 503, unavailableBody
	}
	now := s.now()
	accounts := cloneAccounts(s.last)
	report := Report{SchemaVersion: SchemaVersion, GeneratedAt: now.UTC().Format(time.RFC3339), PoolStatus: s.poolStatus}
	if !s.refreshedAt.IsZero() {
		report.RefreshedAt = s.refreshedAt.UTC().Format(time.RFC3339)
	}
	s.mu.Unlock()

	if !live {
		for i := range accounts {
			if accounts[i].Source == SourceLive {
				accounts[i].Source = SourceCache
			}
		}
	}
	if hasSession {
		accounts = mergeSession(accounts, session, now)
	}
	report.Accounts = accounts
	body, err := json.Marshal(report)
	if err != nil {
		return 500, []byte(`{"error":"encoding_failed"}`)
	}
	return 200, body
}

func (s *Service) sessionObservation() (sessionObservation, bool) {
	s.mu.Lock()
	if s.sessionAt.IsZero() || s.now().Sub(s.sessionAt) >= sessionTTL {
		reader, now := s.sessions, s.now()
		s.mu.Unlock()
		obs, ok := reader(now)
		s.mu.Lock()
		s.sessionAt = now
		s.session = nil
		if ok {
			s.session = &obs
		}
	}
	defer s.mu.Unlock()
	if s.session == nil {
		return sessionObservation{}, false
	}
	return *s.session, true
}

// mergeSession applies the local Codex session log when it is newer than the
// live result, or when the live query failed or never ran.
func mergeSession(accounts []Account, obs sessionObservation, now time.Time) []Account {
	windows := expireResetWindows(obs.windows, now)
	apply := func(a *Account) {
		if a.Status == StatusError {
			if a.Error != ErrNotConfigured {
				a.LiveError = a.Error
			}
		}
		a.Status, a.Error = StatusOK, ""
		a.Source = SourceSessionLog
		a.ObservedAt = obs.observedAt.Format(time.RFC3339)
		a.Windows = windows
		if a.Plan == "" {
			a.Plan = obs.plan
		}
	}
	for i := range accounts {
		a := &accounts[i]
		if a.Provider != ProviderCodex || a.Origin != OriginLocal {
			continue
		}
		observed, err := time.Parse(time.RFC3339, a.ObservedAt)
		if a.Status == StatusError || err != nil || obs.observedAt.After(observed) {
			apply(a)
		}
		return accounts
	}
	a := Account{ID: "local-codex", Provider: ProviderCodex, Origin: OriginLocal}
	apply(&a)
	// Keep local accounts ahead of pool accounts.
	at := 0
	for at < len(accounts) && accounts[at].Origin == OriginLocal {
		at++
	}
	accounts = append(accounts, Account{})
	copy(accounts[at+1:], accounts[at:])
	accounts[at] = a
	return accounts
}

func cloneAccounts(in []Account) []Account {
	out := make([]Account, len(in))
	for i, a := range in {
		out[i] = a
		out[i].MergedPool = append([]int(nil), a.MergedPool...)
		out[i].Windows = append([]Window(nil), a.Windows...)
	}
	return out
}

// candidate is one credential before deduplication.
type candidate struct {
	provider string
	origin   string
	number   int // pool number; 0 for local
	cred     credential
	err      error  // credential could not be read; no token to use
	expired  bool   // recorded expiry passed; the token is never sent
	key      string // dedup key: provider + account identity
}

func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:12])
}

func (s *Service) refresh(ctx context.Context, host HostFunc) ([]Account, string) {
	client := providerClient{host: host, userAgent: s.userAgent}
	now := s.now()
	var candidates []*candidate

	claude, err := s.local.Claude()
	candidates = append(candidates, &candidate{provider: ProviderClaude, origin: OriginLocal, cred: claude, err: err})
	codex, err := s.local.Codex()
	candidates = append(candidates, &candidate{provider: ProviderCodex, origin: OriginLocal, cred: codex, err: err})

	poolStatus := PoolUnavailable
	if host != nil {
		entries, err := host.listPool()
		if err != nil {
			poolStatus = PoolError
		} else {
			poolStatus = PoolOK
			for _, entry := range entries {
				c := &candidate{provider: entry.provider, origin: OriginPool, number: entry.number}
				raw, err := host.authJSON(entry.authIndex)
				if err != nil {
					c.err = errNotConfigured
				} else if entry.provider == ProviderClaude {
					c.cred, c.err = parseClaudePool(raw)
				} else {
					c.cred, c.err = parseCodexPool(raw)
				}
				candidates = append(candidates, c)
			}
		}
	}

	// A token past its recorded expiry would only produce a 401; mark it
	// unusable without a request. The CLI or CPA refreshes it, never this
	// plugin. Expiry is kept separate from err so a known account identity can
	// still merge with a usable credential of the same account.
	for _, c := range candidates {
		if c.err != nil {
			continue
		}
		if !c.cred.expiresAt.IsZero() && !c.cred.expiresAt.After(now) {
			c.expired = true
			continue
		}
		if host == nil {
			c.err = &fetchError{ErrUpstream}
		}
	}

	s.identify(ctx, client, candidates)

	// Group by identity, local first, then pool order.
	type group struct {
		members []*candidate
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, c := range candidates {
		if c.err != nil {
			groups = append(groups, &group{members: []*candidate{c}})
			continue
		}
		if g, ok := byKey[c.key]; ok {
			g.members = append(g.members, c)
			continue
		}
		g := &group{members: []*candidate{c}}
		byKey[c.key] = g
		groups = append(groups, g)
	}

	results := make([]cachedResult, len(groups))
	fresh := make([]bool, len(groups))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, g := range groups {
		lead := g.members[0]
		if lead.err != nil {
			results[i] = cachedResult{at: now, status: StatusError, code: codeOf(lead.err)}
			continue
		}
		usable := false
		for _, m := range g.members {
			if !m.expired {
				usable = true
				break
			}
		}
		if !usable {
			// Every credential of this account is past its recorded expiry:
			// one error card, and no token is sent.
			results[i] = cachedResult{at: now, status: StatusError, code: ErrTokenExpired}
			continue
		}
		if cached, ok := s.cachedResult(lead.key, now); ok {
			results[i] = cached
			continue
		}
		wg.Add(1)
		go func(i int, members []*candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.fetchGroup(ctx, client, members)
			fresh[i] = true
		}(i, g.members)
	}
	wg.Wait()

	accounts := make([]Account, 0, len(groups))
	for i, g := range groups {
		lead := g.members[0]
		res := results[i]
		if fresh[i] && res.code != ErrCanceled {
			s.storeResult(lead.key, res)
		}
		a := Account{Provider: lead.provider, Origin: lead.origin, PoolNumber: lead.number, Plan: lead.cred.plan}
		if lead.origin == OriginLocal {
			a.ID = "local-" + lead.provider
		} else {
			a.ID = "pool-" + strconv.Itoa(lead.number)
		}
		for _, m := range g.members[1:] {
			a.MergedPool = append(a.MergedPool, m.number)
			if a.Plan == "" {
				a.Plan = m.cred.plan
			}
		}
		if res.plan != "" {
			a.Plan = res.plan
		}
		if lead.provider == ProviderClaude && lead.err == nil {
			if p, ok := s.cachedProfile(lead.cred.token, now); ok && p.plan != "" && a.Plan == "" {
				a.Plan = p.plan
			}
		}
		a.Status, a.Error, a.Windows = res.status, res.code, res.windows
		if a.Windows == nil {
			a.Windows = []Window{}
		}
		if res.status == StatusOK {
			a.ObservedAt = res.at.UTC().Format(time.RFC3339)
			a.Source = SourceCache
			if fresh[i] {
				a.Source = SourceLive
			}
		}
		// A local account that is not logged in is reported as such; the
		// page hides it. Pool entries that cannot be read are kept so the
		// numbering stays visible.
		accounts = append(accounts, a)
	}
	return accounts, poolStatus
}

// identify assigns each usable candidate a deduplication key. Codex uses the
// ChatGPT account ID. Claude uses the account UUID from the pool file or from
// /api/oauth/profile, cached by token fingerprint; a token whose identity
// cannot be resolved still deduplicates against identical tokens.
func (s *Service) identify(ctx context.Context, client providerClient, candidates []*candidate) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)
	for _, c := range candidates {
		if c.err != nil {
			continue
		}
		if c.cred.accountID != "" {
			c.key = c.provider + ":" + c.cred.accountID
			continue
		}
		c.key = c.provider + ":token:" + fingerprint(c.cred.token)
		// Never send an expired token just to learn its identity. Without a
		// stored identity the token fingerprint is the best available key.
		if c.expired || c.provider != ProviderClaude {
			continue
		}
		if p, ok := s.cachedProfile(c.cred.token, s.now()); ok {
			c.key = ProviderClaude + ":" + p.accountUUID
			continue
		}
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()
			profile, err := client.claudeProfile(reqCtx, c.cred.token)
			if err != nil {
				if codeOf(err) == ErrTokenExpired {
					c.err = err
				}
				return
			}
			s.storeProfile(c.cred.token, profile)
			c.key = ProviderClaude + ":" + profile.accountUUID
		}(c)
	}
	wg.Wait()
}

// fetchGroup queries one account, trying the next credential of the same
// account when a token is rejected.
func (s *Service) fetchGroup(ctx context.Context, client providerClient, members []*candidate) cachedResult {
	var last cachedResult
	for _, m := range members {
		if m.expired {
			last = cachedResult{at: s.now(), status: StatusError, code: ErrTokenExpired}
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		var windows []Window
		var plan string
		var err error
		if m.provider == ProviderClaude {
			windows, err = client.claudeUsage(reqCtx, m.cred.token)
		} else {
			windows, plan, err = client.codexUsage(reqCtx, m.cred.token, m.cred.accountID)
		}
		cancel()
		if err == nil {
			return cachedResult{at: s.now(), status: StatusOK, plan: plan, windows: windows}
		}
		if ctx.Err() != nil {
			return cachedResult{at: s.now(), status: StatusError, code: ErrCanceled}
		}
		last = cachedResult{at: s.now(), status: StatusError, code: codeOf(err)}
		if last.code != ErrTokenExpired {
			break
		}
	}
	return last
}

func (s *Service) cachedResult(key string, now time.Time) (cachedResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.results[key]
	if !ok {
		return cachedResult{}, false
	}
	ttl := resultTTL
	if res.status != StatusOK {
		ttl = errorTTL
	}
	if now.Sub(res.at) >= ttl {
		delete(s.results, key)
		return cachedResult{}, false
	}
	res.windows = append([]Window(nil), res.windows...)
	return res, true
}

func (s *Service) storeResult(key string, res cachedResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.paused {
		return
	}
	s.results[key] = res
}

func (s *Service) cachedProfile(token string, now time.Time) (claudeProfile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := fingerprint(token)
	entry, ok := s.profiles[fp]
	if !ok || now.Sub(entry.at) >= profileTTL {
		delete(s.profiles, fp)
		return claudeProfile{}, false
	}
	return entry.profile, true
}

func (s *Service) storeProfile(token string, profile claudeProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.paused {
		return
	}
	s.profiles[fingerprint(token)] = profileEntry{at: s.now(), profile: profile}
}
