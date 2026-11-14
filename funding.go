package main

// Monthly zap funding goal.
//
// When FUNDING_GOAL_SATS > 0 the relay publishes a NIP-75 zap goal (kind 9041)
// every month from a dedicated "funding identity" key. Zap receipts (kind 9735)
// for that goal are tallied; until the goal is met (after an optional grace
// period at the start of the month) the relay rejects all client writes and
// advertises itself as payment_required in its NIP-11 document. Once the goal
// is reached writes open up for everyone in the web of trust. On the first of
// the next month everything resets.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/eventstore/lmdb"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip19"
	"github.com/joho/godotenv"
)

// fundingConfig is everything the funding engine needs from the environment.
type fundingConfig struct {
	GoalSats         uint64
	SecretKeyHex     string
	LightningAddress string
	GraceDays        int
	Timezone         string
	Name             string

	// copied from the main relay config
	RelayName  string
	RelayURL   string
	RelayIcon  string
	SeedRelays []string
	// Relays is where zap receipts are delivered to and tallied from (the
	// "relays" tag of the goal and of every zap request).
	Relays []string
}

// defaultFundingRelays are big general-purpose relays that accept zap
// receipts; lightning providers deliver receipts there and the relay tallies
// from them. Override with FUNDING_RELAYS.
var defaultFundingRelays = []string{
	"wss://relay.ditto.pub",
	"wss://relay.primal.net",
	"wss://relay.damus.io",
	"wss://nos.lol",
}

func (c fundingConfig) enabled() bool { return c.GoalSats > 0 }

func (c fundingConfig) goalMsat() uint64 { return c.GoalSats * 1000 }

// lnurlInfo is the subset of the LUD-06/LUD-16 pay response we care about.
type lnurlInfo struct {
	Callback    string
	NostrPubkey nostr.PubKey
	MinSendable uint64 // msats
	MaxSendable uint64 // msats
	ResolvedAt  time.Time
}

func (l lnurlInfo) ok() bool { return l.Callback != "" && l.NostrPubkey != nostr.ZeroPK }

type funding struct {
	cfg   fundingConfig
	sk    nostr.SecretKey
	pk    nostr.PubKey
	loc   *time.Location
	relay *khatru.Relay
	db    *lmdb.LMDBBackend
	pool  *nostr.Pool
	http  *http.Client
	now   func() time.Time

	mu           sync.RWMutex
	lnurl        lnurlInfo
	periodStart  time.Time
	periodEnd    time.Time
	graceUntil   time.Time
	goal         *nostr.Event
	raisedMsat   uint64
	zapCount     int
	counted      map[nostr.ID]struct{}
	contributors map[nostr.PubKey]uint64
	cancelSync   context.CancelFunc

	limiter *ipLimiter
}

// loadFundingConfig reads the FUNDING_* variables. It never fails when the
// feature is disabled (FUNDING_GOAL_SATS unset or 0).
func loadFundingConfig(main Config) fundingConfig {
	godotenv.Load(".env")

	goal, _ := strconv.ParseUint(strings.TrimSpace(getenvDefault("FUNDING_GOAL_SATS", "0")), 10, 64)
	grace, err := strconv.Atoi(strings.TrimSpace(getenvDefault("FUNDING_GRACE_DAYS", "3")))
	if err != nil || grace < 0 {
		grace = 3
	}

	c := fundingConfig{
		GoalSats:         goal,
		SecretKeyHex:     strings.TrimSpace(getenvDefault("FUNDING_SECRET_KEY", "")),
		LightningAddress: strings.TrimSpace(getenvDefault("FUNDING_LIGHTNING_ADDRESS", "")),
		GraceDays:        grace,
		Timezone:         strings.TrimSpace(getenvDefault("FUNDING_TIMEZONE", "UTC")),
		Name:             strings.TrimSpace(getenvDefault("FUNDING_NAME", "")),
		RelayName:        main.RelayName,
		RelayURL:         main.RelayURL,
		RelayIcon:        main.RelayIcon,
		SeedRelays:       main.SeedRelays,
	}
	if fr := strings.TrimSpace(getenvDefault("FUNDING_RELAYS", "")); fr != "" {
		c.Relays = splitAndTrim(fr)
	} else {
		c.Relays = defaultFundingRelays
	}
	if c.Name == "" {
		c.Name = main.RelayName + " Fund"
	}
	return c
}

func getenvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// newFunding validates the configuration and builds the engine. It returns
// nil (and no error) when the feature is disabled.
func newFunding(cfg fundingConfig, relay *khatru.Relay, db *lmdb.LMDBBackend, pool *nostr.Pool) (*funding, error) {
	if !cfg.enabled() {
		return nil, nil
	}
	if cfg.SecretKeyHex == "" {
		return nil, errors.New("FUNDING_SECRET_KEY must be set when FUNDING_GOAL_SATS > 0 (a hex secret key for the relay's funding identity, NOT your own nsec)")
	}
	if strings.HasPrefix(cfg.SecretKeyHex, "nsec1") {
		prefix, value, err := nip19.Decode(cfg.SecretKeyHex)
		if err != nil || prefix != "nsec" {
			return nil, fmt.Errorf("FUNDING_SECRET_KEY: invalid nsec: %v", err)
		}
		if sk, ok := value.(nostr.SecretKey); ok {
			cfg.SecretKeyHex = sk.Hex()
		}
	}
	sk, err := nostr.SecretKeyFromHex(cfg.SecretKeyHex)
	if err != nil {
		return nil, fmt.Errorf("FUNDING_SECRET_KEY: %w", err)
	}
	if name, domain, ok := splitLightningAddress(cfg.LightningAddress); !ok || name == "" || domain == "" {
		return nil, errors.New("FUNDING_LIGHTNING_ADDRESS must be a lightning address like name@domain.com")
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("FUNDING_TIMEZONE: %w", err)
	}

	f := &funding{
		cfg:          cfg,
		sk:           sk,
		pk:           sk.Public(),
		loc:          loc,
		relay:        relay,
		db:           db,
		pool:         pool,
		http:         &http.Client{Timeout: 15 * time.Second},
		now:          time.Now,
		counted:      map[nostr.ID]struct{}{},
		contributors: map[nostr.PubKey]uint64{},
		limiter:      newIPLimiter(10, time.Minute),
	}
	return f, nil
}

// lnurlpEndpoint builds the LUD-16 URL. Tor and local addresses use plain http.
func lnurlpEndpoint(name, domain string) string {
	scheme := "https"
	host := domain
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	if strings.HasSuffix(host, ".onion") || host == "localhost" || host == "127.0.0.1" {
		scheme = "http"
	}
	return scheme + "://" + domain + "/.well-known/lnurlp/" + url.PathEscape(name)
}

func splitLightningAddress(addr string) (name, domain string, ok bool) {
	parts := strings.Split(strings.TrimSpace(addr), "@")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// websiteURL turns wss://relay.example into https://relay.example/.
func websiteURL(relayURL string) string {
	u := strings.TrimSpace(relayURL)
	switch {
	case strings.HasPrefix(u, "wss://"):
		u = "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		u = "http://" + strings.TrimPrefix(u, "ws://")
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u
}

// ---------------------------------------------------------------------------
// period math

// periodFor returns the start (inclusive) and end (exclusive) of the calendar
// month containing now, in the given location.
func periodFor(now time.Time, loc *time.Location) (start, end time.Time) {
	t := now.In(loc)
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	end = start.AddDate(0, 1, 0)
	return start, end
}

func (f *funding) inGrace() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.inGraceLocked()
}

func (f *funding) inGraceLocked() bool {
	return f.now().Before(f.graceUntil)
}

func (f *funding) fundedLocked() bool {
	return f.raisedMsat >= f.cfg.goalMsat()
}

// locked reports whether client writes must currently be rejected.
// Fails open while the engine hasn't set up the current period yet.
func (f *funding) locked() bool {
	if f == nil {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.goal == nil {
		return false
	}
	if f.inGraceLocked() {
		return false
	}
	return !f.fundedLocked()
}

// ---------------------------------------------------------------------------
// lifecycle

// run is the main loop: sets up the current period, resolves the LNURL
// provider, and rolls over on month boundaries. Blocks until ctx is done.
func (f *funding) run(ctx context.Context) {
	log.Printf("⚡ funding goal enabled: %d sats/month, grace %d day(s), timezone %s", f.cfg.GoalSats, f.cfg.GraceDays, f.cfg.Timezone)
	log.Printf("⚡ funding identity: %s (zaps go to %s)", nip19.EncodeNpub(f.pk), f.cfg.LightningAddress)
	if !publiclyReachable(f.cfg.RelayURL) {
		log.Printf("⚠️  funding: RELAY_URL %s is not reachable by lightning providers; zap receipts will only arrive via the seed relays", f.cfg.RelayURL)
	}

	f.ensureProfile(ctx)
	f.startPeriod(ctx)

	// resolve the LNURL provider, retrying until it works
	go func() {
		for {
			if err := f.resolveLNURL(ctx); err != nil {
				log.Printf("⚠️  funding: could not resolve %s: %v (retrying in 30s)", f.cfg.LightningAddress, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
					continue
				}
			}
			f.restartReceiptSync(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(6 * time.Hour):
			}
		}
	}()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			start, _ := periodFor(f.now(), f.loc)
			f.mu.RLock()
			cur := f.periodStart
			f.mu.RUnlock()
			if !start.Equal(cur) {
				log.Println("📅 funding: new month, resetting goal")
				f.startPeriod(ctx)
				f.restartReceiptSync(ctx)
			}
		}
	}
}

// startPeriod resets the tally for the month containing now and finds or
// creates this month's goal event.
func (f *funding) startPeriod(ctx context.Context) {
	start, end := periodFor(f.now(), f.loc)

	f.mu.Lock()
	f.periodStart = start
	f.periodEnd = end
	f.graceUntil = start.AddDate(0, 0, f.cfg.GraceDays)
	f.raisedMsat = 0
	f.zapCount = 0
	f.counted = map[nostr.ID]struct{}{}
	f.contributors = map[nostr.PubKey]uint64{}
	f.goal = nil
	f.mu.Unlock()

	goal := f.findGoal(start, end)
	if goal == nil {
		g, err := f.createGoal(ctx, start, end)
		if err != nil {
			log.Printf("⚠️  funding: could not create goal event: %v", err)
			return
		}
		goal = g
		log.Printf("🎯 funding: published goal %s for %s", nip19.EncodeNevent(goal.ID, []string{f.cfg.RelayURL}, f.pk), start.Format("January 2006"))
	} else {
		log.Printf("🎯 funding: reusing goal %s for %s", goal.ID.Hex(), start.Format("January 2006"))
	}

	f.mu.Lock()
	f.goal = goal
	f.mu.Unlock()

	if f.inGrace() {
		log.Printf("⏳ funding: grace period until %s, writes are open", f.graceUntil.Format(time.RFC1123))
	}
}

// findGoal looks for a goal event we already published for this period.
func (f *funding) findGoal(start, end time.Time) *nostr.Event {
	filter := nostr.Filter{
		Kinds:   []nostr.Kind{nostr.KindZapGoal},
		Authors: []nostr.PubKey{f.pk},
		Since:   nostr.Timestamp(start.Unix()),
		Until:   nostr.Timestamp(end.Unix() - 1),
		Limit:   10,
	}
	want := strconv.FormatUint(f.cfg.goalMsat(), 10)
	var best *nostr.Event
	for evt := range f.db.QueryEvents(filter, 10) {
		e := evt
		// a goal published for a different FUNDING_GOAL_SATS is stale
		if amt := e.Tags.Find("amount"); amt == nil || amt[1] != want {
			continue
		}
		if best == nil || e.CreatedAt > best.CreatedAt {
			best = &e
		}
	}
	return best
}

// publiclyReachable reports whether a relay URL can be reached by a
// lightning provider on the internet (i.e. it is a wss:// URL and not localhost).
func publiclyReachable(relayURL string) bool {
	u := strings.ToLower(strings.TrimSpace(relayURL))
	if !strings.HasPrefix(u, "wss://") {
		return false
	}
	host := strings.TrimPrefix(u, "wss://")
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}

// goalRelays is the relay list put in the goal event and in zap requests:
// the relays a lightning provider should deliver receipts to. Our own URL
// goes first when a provider can reach it; otherwise it is left out (a
// provider that cannot reach the first relay may give up on the rest).
func (f *funding) goalRelays() []string {
	var relays []string
	if publiclyReachable(f.cfg.RelayURL) {
		relays = append(relays, f.cfg.RelayURL)
	}
	for _, r := range f.cfg.Relays {
		if len(relays) >= 6 {
			break
		}
		if r != f.cfg.RelayURL && !containsString(relays, r) {
			relays = append(relays, r)
		}
	}
	return relays
}

func (f *funding) createGoal(ctx context.Context, start, end time.Time) (*nostr.Event, error) {
	month := start.Format("January 2006")
	site := websiteURL(f.cfg.RelayURL)
	content := fmt.Sprintf(
		"%s — funding goal for %s.\n\n"+
			"This relay is community funded. Once %s sats are raised this month, everyone in the web of trust can write to %s. "+
			"Zap this goal or visit %s to contribute.",
		f.cfg.RelayName, month, formatSats(f.cfg.GoalSats), f.cfg.RelayURL, site)

	relaysTag := append(nostr.Tag{"relays"}, f.goalRelays()...)
	evt := nostr.Event{
		Kind:      nostr.KindZapGoal,
		CreatedAt: nostr.Timestamp(f.now().Unix()),
		Content:   content,
		Tags: nostr.Tags{
			{"amount", strconv.FormatUint(f.cfg.goalMsat(), 10)},
			relaysTag,
			{"closed_at", strconv.FormatInt(end.Unix(), 10)},
			{"summary", fmt.Sprintf("Keep %s writable in %s", f.cfg.RelayName, month)},
			{"r", site},
			{"alt", fmt.Sprintf("Zap goal: %s sats for %s (%s)", formatSats(f.cfg.GoalSats), f.cfg.RelayName, month)},
		},
	}
	if f.cfg.RelayIcon != "" {
		evt.Tags = append(evt.Tags, nostr.Tag{"image", f.cfg.RelayIcon})
	}
	if err := evt.Sign(f.sk); err != nil {
		return nil, err
	}
	if err := f.db.SaveEvent(evt); err != nil && !errors.Is(err, eventstore.ErrDupEvent) {
		return nil, err
	}
	f.relay.BroadcastEvent(evt)
	f.publishToSeeds(ctx, evt)
	return &evt, nil
}

// ensureProfile publishes (or refreshes) the kind 0 profile of the funding
// identity so that clients zapping the goal can resolve its lightning address.
func (f *funding) ensureProfile(ctx context.Context) {
	site := websiteURL(f.cfg.RelayURL)
	want := map[string]string{
		"name":         f.cfg.Name,
		"display_name": f.cfg.Name,
		"about":        fmt.Sprintf("Monthly funding goal for %s (%s). Zap the goal to keep the relay writable for everyone in the web of trust.", f.cfg.RelayName, f.cfg.RelayURL),
		"picture":      f.cfg.RelayIcon,
		"lud16":        f.cfg.LightningAddress,
		"website":      site,
	}

	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindProfileMetadata}, Authors: []nostr.PubKey{f.pk}, Limit: 1}
	for evt := range f.db.QueryEvents(filter, 1) {
		var have map[string]string
		if err := json.Unmarshal([]byte(evt.Content), &have); err == nil {
			same := true
			for k, v := range want {
				if have[k] != v {
					same = false
					break
				}
			}
			if same {
				return
			}
		}
	}

	content, _ := json.Marshal(want)
	evt := nostr.Event{
		Kind:      nostr.KindProfileMetadata,
		CreatedAt: nostr.Timestamp(f.now().Unix()),
		Content:   string(content),
		Tags:      nostr.Tags{},
	}
	if err := evt.Sign(f.sk); err != nil {
		log.Printf("⚠️  funding: sign profile: %v", err)
		return
	}
	if _, err := f.db.ReplaceEvent(evt); err != nil && !errors.Is(err, eventstore.ErrDupEvent) {
		log.Printf("⚠️  funding: save profile: %v", err)
		return
	}
	f.relay.BroadcastEvent(evt)
	f.publishToSeeds(ctx, evt)
	log.Println("👤 funding: published funding identity profile")
}

func (f *funding) publishToSeeds(ctx context.Context, evt nostr.Event) {
	urls := f.cfg.SeedRelays
	if len(urls) == 0 {
		return
	}
	go func() {
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		okCount := 0
		for res := range f.pool.PublishMany(tctx, urls, evt) {
			if res.Error == nil {
				okCount++
			}
		}
		log.Printf("📡 funding: kind %d published to %d/%d seed relays", evt.Kind, okCount, len(urls))
	}()
}

// ---------------------------------------------------------------------------
// LNURL

func (f *funding) resolveLNURL(ctx context.Context) error {
	name, domain, _ := splitLightningAddress(f.cfg.LightningAddress)
	endpoint := lnurlpEndpoint(name, domain)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", endpoint, resp.StatusCode)
	}

	var doc struct {
		Status      string `json:"status"`
		Reason      string `json:"reason"`
		Callback    string `json:"callback"`
		AllowsNostr bool   `json:"allowsNostr"`
		NostrPubkey string `json:"nostrPubkey"`
		MinSendable uint64 `json:"minSendable"`
		MaxSendable uint64 `json:"maxSendable"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("decode lnurlp response: %w", err)
	}
	if strings.EqualFold(doc.Status, "ERROR") {
		return fmt.Errorf("lnurl error: %s", doc.Reason)
	}
	if doc.Callback == "" {
		return errors.New("lnurlp response has no callback")
	}
	if !doc.AllowsNostr || doc.NostrPubkey == "" {
		return fmt.Errorf("%s does not support nostr zaps (allowsNostr/nostrPubkey missing) — use a zap-capable lightning address", f.cfg.LightningAddress)
	}
	provider, err := nostr.PubKeyFromHex(doc.NostrPubkey)
	if err != nil {
		return fmt.Errorf("invalid nostrPubkey from lnurl server: %w", err)
	}

	info := lnurlInfo{
		Callback:    doc.Callback,
		NostrPubkey: provider,
		MinSendable: doc.MinSendable,
		MaxSendable: doc.MaxSendable,
		ResolvedAt:  f.now(),
	}
	if info.MinSendable == 0 {
		info.MinSendable = 1000
	}
	if info.MaxSendable == 0 {
		info.MaxSendable = 100_000_000_000
	}

	f.mu.Lock()
	changed := f.lnurl.NostrPubkey != info.NostrPubkey
	f.lnurl = info
	f.mu.Unlock()
	if changed {
		log.Printf("⚡ funding: zap provider for %s is %s", f.cfg.LightningAddress, provider.Hex())
	}
	return nil
}

// requestInvoice asks the LNURL callback for a bolt11 invoice for the given
// (already validated) zap request.
func (f *funding) requestInvoice(ctx context.Context, zapReq nostr.Event) (string, error) {
	f.mu.RLock()
	ln := f.lnurl
	f.mu.RUnlock()
	if !ln.ok() {
		return "", errors.New("lightning address not resolved yet, try again shortly")
	}
	amountTag := zapReq.Tags.Find("amount")
	if amountTag == nil {
		return "", errors.New("zap request has no amount")
	}
	reqJSON, err := json.Marshal(zapReq)
	if err != nil {
		return "", err
	}

	cb, err := url.Parse(ln.Callback)
	if err != nil {
		return "", fmt.Errorf("bad callback url: %w", err)
	}
	q := cb.Query()
	q.Set("amount", amountTag[1])
	q.Set("nostr", string(reqJSON))
	cb.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cb.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var doc struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		PR     string `json:"pr"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("decode invoice response (HTTP %d): %w", resp.StatusCode, err)
	}
	if strings.EqualFold(doc.Status, "ERROR") || doc.PR == "" {
		if doc.Reason == "" {
			doc.Reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("lightning provider refused: %s", doc.Reason)
	}
	return doc.PR, nil
}

// ---------------------------------------------------------------------------
// zap receipts

// validateGoalZapReceipt checks that ev is a zap receipt for goalID, signed by
// the expected LNURL provider, and returns the embedded zap request.
func validateGoalZapReceipt(ev nostr.Event, provider, fundingPk nostr.PubKey, goalID nostr.ID, start, end time.Time) (nostr.Event, bool) {
	var none nostr.Event
	if ev.Kind != nostr.KindZap {
		return none, false
	}
	if goalID == nostr.ZeroID || provider == nostr.ZeroPK {
		return none, false
	}
	if ev.PubKey != provider {
		return none, false
	}
	if e := ev.Tags.Find("e"); e == nil || len(e) < 2 || e[1] != goalID.Hex() {
		return none, false
	}
	if p := ev.Tags.Find("p"); p == nil || len(p) < 2 || p[1] != fundingPk.Hex() {
		return none, false
	}
	desc := ev.Tags.Find("description")
	if desc == nil || len(desc) < 2 {
		return none, false
	}
	var req nostr.Event
	if err := json.Unmarshal([]byte(desc[1]), &req); err != nil {
		return none, false
	}
	if req.Kind != nostr.KindZapRequest || !req.VerifySignature() {
		return none, false
	}
	if e := req.Tags.Find("e"); e == nil || len(e) < 2 || e[1] != goalID.Hex() {
		return none, false
	}
	if p := req.Tags.Find("p"); p == nil || len(p) < 2 || p[1] != fundingPk.Hex() {
		return none, false
	}
	if zapRequestAmount(req) == 0 {
		return none, false
	}
	ts := ev.CreatedAt.Time()
	if ts.Before(start) || !ts.Before(end) {
		return none, false
	}
	return req, true
}

func zapRequestAmount(req nostr.Event) uint64 {
	tag := req.Tags.Find("amount")
	if tag == nil || len(tag) < 2 {
		return 0
	}
	amt, err := strconv.ParseUint(tag[1], 10, 64)
	if err != nil {
		return 0
	}
	return amt
}

// isGoalZapReceipt reports whether ev is a valid receipt for the current goal.
func (f *funding) isGoalZapReceipt(ev nostr.Event) (nostr.Event, bool) {
	if f == nil || ev.Kind != nostr.KindZap {
		return nostr.Event{}, false
	}
	f.mu.RLock()
	provider := f.lnurl.NostrPubkey
	goalID := nostr.ZeroID
	if f.goal != nil {
		goalID = f.goal.ID
	}
	start, end := f.periodStart, f.periodEnd
	f.mu.RUnlock()
	return validateGoalZapReceipt(ev, provider, f.pk, goalID, start, end)
}

// tally counts a receipt once. Returns true if it was newly counted.
func (f *funding) tally(ev nostr.Event) bool {
	req, ok := f.isGoalZapReceipt(ev)
	if !ok {
		return false
	}
	amount := zapRequestAmount(req)

	f.mu.Lock()
	if _, seen := f.counted[ev.ID]; seen {
		f.mu.Unlock()
		return false
	}
	wasFunded := f.fundedLocked()
	f.counted[ev.ID] = struct{}{}
	f.raisedMsat += amount
	f.zapCount++
	f.contributors[req.PubKey] += amount
	raised, goal := f.raisedMsat, f.cfg.goalMsat()
	nowFunded := f.fundedLocked()
	f.mu.Unlock()

	log.Printf("⚡ funding: +%s sats from %s (%s / %s sats, %d%%)",
		formatSats(amount/1000), nip19.EncodeNpub(req.PubKey), formatSats(raised/1000), formatSats(goal/1000), pct(raised, goal))
	if nowFunded && !wasFunded {
		log.Println("🎉 funding: monthly goal reached, relay is writable!")
	}
	return true
}

// onEventSaved is wired into relay.OnEventSaved.
func (f *funding) onEventSaved(_ context.Context, ev nostr.Event) {
	if f == nil {
		return
	}
	f.tally(ev)
}

// restartReceiptSync replays receipts stored locally for the current goal and
// then follows the goal's relays for new ones.
func (f *funding) restartReceiptSync(ctx context.Context) {
	f.mu.Lock()
	if f.cancelSync != nil {
		f.cancelSync()
		f.cancelSync = nil
	}
	goal := f.goal
	ln := f.lnurl
	start := f.periodStart
	f.mu.Unlock()

	if goal == nil || !ln.ok() {
		return
	}

	// replay from our own store
	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindZap},
		Tags:  nostr.TagMap{"e": {goal.ID.Hex()}},
		Since: nostr.Timestamp(start.Unix()),
		Limit: 10000,
	}
	replayed := 0
	for evt := range f.db.QueryEvents(filter, 10000) {
		if f.tally(evt) {
			replayed++
		}
	}
	if replayed > 0 {
		log.Printf("⚡ funding: replayed %d receipt(s) from local store", replayed)
	}

	// follow the goal's relays for new receipts
	subCtx, cancel := context.WithCancel(ctx)
	f.mu.Lock()
	f.cancelSync = cancel
	f.mu.Unlock()

	// follow the other goal relays (not ourselves: receipts delivered to us
	// directly already go through OnEvent)
	var urls []string
	for _, u := range f.goalRelays() {
		if u != f.cfg.RelayURL {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return
	}
	go func() {
		filter := nostr.Filter{
			Kinds: []nostr.Kind{nostr.KindZap},
			Tags:  nostr.TagMap{"e": {goal.ID.Hex()}},
			Since: nostr.Timestamp(start.Unix()),
		}
		for ev := range f.pool.SubscribeMany(subCtx, urls, filter, nostr.SubscriptionOptions{}) {
			if _, ok := f.isGoalZapReceipt(ev.Event); !ok {
				continue
			}
			// goes through OnEvent -> store -> OnEventSaved -> tally
			if _, err := f.relay.AddEvent(subCtx, ev.Event); err != nil {
				nostr.InfoLogger.Printf("funding: add receipt %s: %v", ev.ID.Hex(), err)
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// NIP-11

// overwriteRelayInformation is wired into relay.OverwriteRelayInformation.
func (f *funding) overwriteRelayInformation(_ context.Context, _ *http.Request, info nip11.RelayInformationDocument) nip11.RelayInformationDocument {
	if f == nil {
		return info
	}
	lim := nip11.RelayLimitationDocument{}
	if info.Limitation != nil {
		lim = *info.Limitation
	}
	lim.RestrictedWrites = true
	info.PaymentsURL = websiteURL(f.cfg.RelayURL)

	if f.locked() {
		lim.PaymentRequired = true
		fees := &nip11.RelayFeesDocument{}
		fees.Subscription = append(fees.Subscription, struct {
			Amount int    `json:"amount"`
			Unit   string `json:"unit"`
			Period int    `json:"period"`
		}{Amount: int(f.cfg.goalMsat()), Unit: "msats", Period: 30 * 24 * 3600})
		info.Fees = fees
	} else {
		lim.PaymentRequired = false
		info.Fees = nil
	}
	info.Limitation = &lim
	return info
}

// rejectReason is the OK message returned to clients while locked.
func (f *funding) rejectReason() string {
	f.mu.RLock()
	raised, goal := f.raisedMsat, f.cfg.goalMsat()
	f.mu.RUnlock()
	return fmt.Sprintf("this relay is community funded and this month's zap goal is not met yet (%s/%s sats) — zap it at %s to unlock writes",
		formatSats(raised/1000), formatSats(goal/1000), websiteURL(f.cfg.RelayURL))
}

// ---------------------------------------------------------------------------
// HTTP

type fundingContributor struct {
	Pubkey  string `json:"pubkey"`
	Npub    string `json:"npub"`
	Name    string `json:"name,omitempty"`
	Picture string `json:"picture,omitempty"`
	Sats    uint64 `json:"sats"`
	Anon    bool   `json:"anonymous,omitempty"`
}

type fundingStatus struct {
	Enabled          bool                 `json:"enabled"`
	GoalSats         uint64               `json:"goal_sats"`
	RaisedSats       uint64               `json:"raised_sats"`
	Percent          int                  `json:"percent"`
	Funded           bool                 `json:"funded"`
	Locked           bool                 `json:"locked"`
	InGrace          bool                 `json:"in_grace"`
	GraceUntil       int64                `json:"grace_until"`
	PeriodStart      int64                `json:"period_start"`
	PeriodEnd        int64                `json:"period_end"`
	Timezone         string               `json:"timezone"`
	GoalEventID      string               `json:"goal_event_id,omitempty"`
	GoalNevent       string               `json:"goal_nevent,omitempty"`
	FundingPubkey    string               `json:"funding_pubkey"`
	FundingNpub      string               `json:"funding_npub"`
	LightningAddress string               `json:"lightning_address"`
	RelayURL         string               `json:"relay_url"`
	ZapCount         int                  `json:"zap_count"`
	Relays           []string             `json:"relays"`
	Contributors     []fundingContributor `json:"contributors"`
	Now              int64                `json:"now"`
}

func (f *funding) status() fundingStatus {
	f.mu.RLock()
	s := fundingStatus{
		Enabled:          true,
		GoalSats:         f.cfg.GoalSats,
		RaisedSats:       f.raisedMsat / 1000,
		Percent:          pct(f.raisedMsat, f.cfg.goalMsat()),
		Funded:           f.fundedLocked(),
		InGrace:          f.inGraceLocked(),
		GraceUntil:       f.graceUntil.Unix(),
		PeriodStart:      f.periodStart.Unix(),
		PeriodEnd:        f.periodEnd.Unix(),
		Timezone:         f.cfg.Timezone,
		FundingPubkey:    f.pk.Hex(),
		FundingNpub:      nip19.EncodeNpub(f.pk),
		LightningAddress: f.cfg.LightningAddress,
		RelayURL:         f.cfg.RelayURL,
		ZapCount:         f.zapCount,
		Relays:           f.goalRelays(),
		Now:              f.now().Unix(),
	}
	if f.goal != nil {
		s.GoalEventID = f.goal.ID.Hex()
		s.GoalNevent = nip19.EncodeNevent(f.goal.ID, []string{f.cfg.RelayURL}, f.pk)
	}
	type kv struct {
		pk  nostr.PubKey
		amt uint64
	}
	list := make([]kv, 0, len(f.contributors))
	for pk, amt := range f.contributors {
		list = append(list, kv{pk, amt})
	}
	f.mu.RUnlock()

	s.Locked = f.locked()

	sort.Slice(list, func(i, j int) bool { return list[i].amt > list[j].amt })
	if len(list) > 10 {
		list = list[:10]
	}
	s.Contributors = make([]fundingContributor, 0, len(list))
	for _, c := range list {
		fc := fundingContributor{Pubkey: c.pk.Hex(), Npub: nip19.EncodeNpub(c.pk), Sats: c.amt / 1000}
		if c.pk == f.pk {
			fc.Anon = true
			fc.Name = "Anonymous"
		} else {
			fc.Name, fc.Picture = f.lookupProfile(c.pk)
		}
		s.Contributors = append(s.Contributors, fc)
	}
	return s
}

func (f *funding) lookupProfile(pk nostr.PubKey) (name, picture string) {
	if f.db == nil {
		return "", ""
	}
	filter := nostr.Filter{Kinds: []nostr.Kind{nostr.KindProfileMetadata}, Authors: []nostr.PubKey{pk}, Limit: 1}
	for evt := range f.db.QueryEvents(filter, 1) {
		var meta struct {
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Picture     string `json:"picture"`
		}
		if err := json.Unmarshal([]byte(evt.Content), &meta); err == nil {
			name = meta.DisplayName
			if name == "" {
				name = meta.Name
			}
			picture = meta.Picture
		}
	}
	return name, picture
}

func (f *funding) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(f.status())
}

type invoiceRequest struct {
	AmountSats uint64          `json:"amount_sats"`
	Comment    string          `json:"comment"`
	ZapRequest json.RawMessage `json:"zap_request"`
}

func (f *funding) handleInvoice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	fail := func(code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}

	if !f.limiter.allow(khatru.GetIPFromRequest(r)) {
		fail(http.StatusTooManyRequests, "too many invoice requests, slow down")
		return
	}

	var in invoiceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&in); err != nil {
		fail(http.StatusBadRequest, "invalid JSON body")
		return
	}

	f.mu.RLock()
	goal := f.goal
	ln := f.lnurl
	f.mu.RUnlock()
	if goal == nil {
		fail(http.StatusServiceUnavailable, "funding goal not ready yet")
		return
	}
	if !ln.ok() {
		fail(http.StatusServiceUnavailable, "lightning address not resolved yet, try again shortly")
		return
	}

	if in.AmountSats < 1 {
		fail(http.StatusBadRequest, "amount_sats must be at least 1")
		return
	}
	msat := in.AmountSats * 1000
	if msat < ln.MinSendable {
		fail(http.StatusBadRequest, fmt.Sprintf("minimum zap is %d sats", (ln.MinSendable+999)/1000))
		return
	}
	if msat > ln.MaxSendable {
		fail(http.StatusBadRequest, fmt.Sprintf("maximum zap is %d sats", ln.MaxSendable/1000))
		return
	}
	if len(in.Comment) > 280 {
		in.Comment = in.Comment[:280]
	}

	var zapReq nostr.Event
	if len(in.ZapRequest) > 0 {
		// browser-signed (NIP-07) zap request: verify it targets our goal
		if err := json.Unmarshal(in.ZapRequest, &zapReq); err != nil {
			fail(http.StatusBadRequest, "invalid zap_request")
			return
		}
		if zapReq.Kind != nostr.KindZapRequest || !zapReq.VerifySignature() {
			fail(http.StatusBadRequest, "zap_request must be a validly signed kind 9734")
			return
		}
		if e := zapReq.Tags.Find("e"); e == nil || len(e) < 2 || e[1] != goal.ID.Hex() {
			fail(http.StatusBadRequest, "zap_request must reference the current goal event")
			return
		}
		if p := zapReq.Tags.Find("p"); p == nil || len(p) < 2 || p[1] != f.pk.Hex() {
			fail(http.StatusBadRequest, "zap_request must be addressed to the funding identity")
			return
		}
		if zapRequestAmount(zapReq) != msat {
			fail(http.StatusBadRequest, "zap_request amount does not match amount_sats")
			return
		}
		if rl := zapReq.Tags.Find("relays"); rl == nil || !containsAny(rl[1:], f.goalRelays()) {
			fail(http.StatusBadRequest, "zap_request relays tag must include at least one of: "+strings.Join(f.goalRelays(), ", "))
			return
		}
	} else {
		// anonymous zap request signed by the funding identity itself
		zapReq = nostr.Event{
			Kind:      nostr.KindZapRequest,
			CreatedAt: nostr.Timestamp(f.now().Unix()),
			Content:   in.Comment,
			Tags: nostr.Tags{
				append(nostr.Tag{"relays"}, f.goalRelays()...),
				{"amount", strconv.FormatUint(msat, 10)},
				{"p", f.pk.Hex()},
				{"e", goal.ID.Hex()},
			},
		}
		if err := zapReq.Sign(f.sk); err != nil {
			fail(http.StatusInternalServerError, "could not sign zap request")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	pr, err := f.requestInvoice(ctx, zapReq)
	if err != nil {
		log.Printf("⚠️  funding: invoice request failed: %v", err)
		fail(http.StatusBadGateway, err.Error())
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"pr":            pr,
		"amount_sats":   in.AmountSats,
		"goal_event_id": goal.ID.Hex(),
		"zap_request":   zapReq,
	})
}

// registerRoutes attaches the funding endpoints to the relay's router.
func (f *funding) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /funding.json", f.handleStatus)
	mux.HandleFunc("POST /funding/invoice", f.handleInvoice)
	mux.HandleFunc("OPTIONS /funding/invoice", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
}

// ---------------------------------------------------------------------------
// helpers

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(list, wanted []string) bool {
	for _, w := range wanted {
		if containsString(list, w) {
			return true
		}
	}
	return false
}

func pct(raised, goal uint64) int {
	if goal == 0 {
		return 0
	}
	p := raised * 100 / goal
	if p > 100 {
		p = 100
	}
	return int(p)
}

// formatSats renders 100000 as "100,000".
func formatSats(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// ipLimiter is a tiny fixed-window per-IP rate limiter for the invoice endpoint.
type ipLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*ipWindow
}

type ipWindow struct {
	start time.Time
	count int
}

func newIPLimiter(max int, window time.Duration) *ipLimiter {
	return &ipLimiter{max: max, window: window, hits: map[string]*ipWindow{}}
}

func (l *ipLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[ip]
	if w == nil || now.Sub(w.start) > l.window {
		l.hits[ip] = &ipWindow{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.max
}
