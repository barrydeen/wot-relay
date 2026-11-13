package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
)

func TestPeriodFor(t *testing.T) {
	utc := time.UTC
	ny, _ := time.LoadLocation("America/New_York")

	cases := []struct {
		name      string
		now       time.Time
		loc       *time.Location
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			"mid month utc",
			time.Date(2026, 9, 15, 12, 0, 0, 0, utc), utc,
			time.Date(2026, 9, 1, 0, 0, 0, 0, utc), time.Date(2026, 10, 1, 0, 0, 0, 0, utc),
		},
		{
			"year boundary",
			time.Date(2026, 12, 31, 23, 59, 59, 0, utc), utc,
			time.Date(2026, 12, 1, 0, 0, 0, 0, utc), time.Date(2027, 1, 1, 0, 0, 0, 0, utc),
		},
		{
			"non-utc zone: 03:00 UTC on the 1st is still the previous month in New York",
			time.Date(2026, 10, 1, 3, 0, 0, 0, utc), ny,
			time.Date(2026, 9, 1, 0, 0, 0, 0, ny), time.Date(2026, 10, 1, 0, 0, 0, 0, ny),
		},
	}
	for _, c := range cases {
		start, end := periodFor(c.now, c.loc)
		if !start.Equal(c.wantStart) || !end.Equal(c.wantEnd) {
			t.Errorf("%s: got [%s, %s) want [%s, %s)", c.name, start, end, c.wantStart, c.wantEnd)
		}
	}
}

// newTestFunding builds an engine with no relay/db/pool, a fixed clock, a
// provider key and a goal event for September 2026.
func newTestFunding(t *testing.T, goalSats uint64, graceDays int) (f *funding, providerSK nostr.SecretKey) {
	t.Helper()
	sk := nostr.Generate()
	providerSK = nostr.Generate()

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	fixedNow := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	f = &funding{
		cfg: fundingConfig{
			GoalSats:         goalSats,
			GraceDays:        graceDays,
			Timezone:         "UTC",
			LightningAddress: "fund@example.com",
			RelayName:        "Test WoT",
			RelayURL:         "wss://relay.test",
			SeedRelays:       []string{"wss://seed.one", "wss://seed.two"},
			Relays:           []string{"wss://seed.one", "wss://seed.two"},
		},
		sk:           sk,
		pk:           sk.Public(),
		loc:          time.UTC,
		now:          func() time.Time { return fixedNow },
		counted:      map[nostr.ID]struct{}{},
		contributors: map[nostr.PubKey]uint64{},
		limiter:      newIPLimiter(10, time.Minute),
		periodStart:  start,
		periodEnd:    end,
		graceUntil:   start.AddDate(0, 0, graceDays),
		lnurl:        lnurlInfo{Callback: "https://example.com/cb", NostrPubkey: providerSK.Public(), MinSendable: 1000, MaxSendable: 1_000_000_000},
	}

	goal := nostr.Event{
		Kind:      nostr.KindZapGoal,
		CreatedAt: nostr.Timestamp(start.Unix() + 60),
		Content:   "goal",
		Tags: nostr.Tags{
			{"amount", strconv.FormatUint(goalSats*1000, 10)},
			{"relays", "wss://relay.test"},
			{"closed_at", strconv.FormatInt(end.Unix(), 10)},
		},
	}
	if err := goal.Sign(sk); err != nil {
		t.Fatal(err)
	}
	f.goal = &goal
	return f, providerSK
}

type receiptOpts struct {
	senderSK   *nostr.SecretKey // nil = fresh key
	amountMsat uint64
	goalID     nostr.ID
	recipient  nostr.PubKey
	createdAt  nostr.Timestamp
	breakSig   bool
}

// makeReceipt builds a kind 9734 zap request signed by the sender and a kind
// 9735 receipt signed by the provider, the way an LNURL server would.
func makeReceipt(t *testing.T, providerSK nostr.SecretKey, o receiptOpts) nostr.Event {
	t.Helper()
	var senderSK nostr.SecretKey
	if o.senderSK != nil {
		senderSK = *o.senderSK
	} else {
		senderSK = nostr.Generate()
	}
	req := nostr.Event{
		Kind:      nostr.KindZapRequest,
		CreatedAt: o.createdAt,
		Content:   "zap!",
		Tags: nostr.Tags{
			{"relays", "wss://relay.test"},
			{"amount", strconv.FormatUint(o.amountMsat, 10)},
			{"p", o.recipient.Hex()},
			{"e", o.goalID.Hex()},
		},
	}
	if err := req.Sign(senderSK); err != nil {
		t.Fatal(err)
	}
	if o.breakSig {
		req.Content = "tampered"
	}
	desc, _ := json.Marshal(req)

	receipt := nostr.Event{
		Kind:      nostr.KindZap,
		CreatedAt: o.createdAt,
		Tags: nostr.Tags{
			{"p", o.recipient.Hex()},
			{"e", o.goalID.Hex()},
			{"bolt11", "lnbc1fake"},
			{"description", string(desc)},
		},
	}
	if err := receipt.Sign(providerSK); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestValidateGoalZapReceipt(t *testing.T) {
	f, providerSK := newTestFunding(t, 100_000, 0)
	inPeriod := nostr.Timestamp(f.periodStart.Unix() + 3600)

	good := makeReceipt(t, providerSK, receiptOpts{amountMsat: 21_000_000, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod})
	if req, ok := f.isGoalZapReceipt(good); !ok {
		t.Fatal("valid receipt rejected")
	} else if zapRequestAmount(req) != 21_000_000 {
		t.Fatalf("amount = %d", zapRequestAmount(req))
	}

	t.Run("wrong provider key", func(t *testing.T) {
		other := nostr.Generate()
		ev := makeReceipt(t, other, receiptOpts{amountMsat: 1000, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt from unknown provider accepted")
		}
	})
	t.Run("wrong goal", func(t *testing.T) {
		ev := makeReceipt(t, providerSK, receiptOpts{amountMsat: 1000, goalID: nostr.ID{1, 2, 3}, recipient: f.pk, createdAt: inPeriod})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt for another event accepted")
		}
	})
	t.Run("wrong recipient", func(t *testing.T) {
		ev := makeReceipt(t, providerSK, receiptOpts{amountMsat: 1000, goalID: f.goal.ID, recipient: nostr.Generate().Public(), createdAt: inPeriod})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt for another recipient accepted")
		}
	})
	t.Run("tampered zap request", func(t *testing.T) {
		ev := makeReceipt(t, providerSK, receiptOpts{amountMsat: 1000, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod, breakSig: true})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt with invalid embedded request accepted")
		}
	})
	t.Run("out of period", func(t *testing.T) {
		ev := makeReceipt(t, providerSK, receiptOpts{amountMsat: 1000, goalID: f.goal.ID, recipient: f.pk, createdAt: nostr.Timestamp(f.periodEnd.Unix())})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt after period end accepted")
		}
		ev = makeReceipt(t, providerSK, receiptOpts{amountMsat: 1000, goalID: f.goal.ID, recipient: f.pk, createdAt: nostr.Timestamp(f.periodStart.Unix() - 1)})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt before period start accepted")
		}
	})
	t.Run("zero amount", func(t *testing.T) {
		ev := makeReceipt(t, providerSK, receiptOpts{amountMsat: 0, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod})
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("zero-amount receipt accepted")
		}
	})
	t.Run("not a zap", func(t *testing.T) {
		ev := nostr.Event{Kind: nostr.KindTextNote, CreatedAt: inPeriod, Content: "hi"}
		_ = ev.Sign(providerSK)
		if _, ok := f.isGoalZapReceipt(ev); ok {
			t.Fatal("text note accepted as receipt")
		}
	})
	t.Run("no provider resolved yet", func(t *testing.T) {
		f2, _ := newTestFunding(t, 100_000, 0)
		f2.lnurl = lnurlInfo{}
		ev := makeReceipt(t, providerSK, receiptOpts{amountMsat: 1000, goalID: f2.goal.ID, recipient: f2.pk, createdAt: inPeriod})
		if _, ok := f2.isGoalZapReceipt(ev); ok {
			t.Fatal("receipt accepted with no provider key")
		}
	})
}

func TestTallyAndLock(t *testing.T) {
	f, providerSK := newTestFunding(t, 100_000, 0)
	inPeriod := nostr.Timestamp(f.periodStart.Unix() + 3600)

	if !f.locked() {
		t.Fatal("should be locked with nothing raised and no grace")
	}
	if f.status().Percent != 0 {
		t.Fatal("percent should be 0")
	}

	alice := nostr.Generate()
	r1 := makeReceipt(t, providerSK, receiptOpts{senderSK: &alice, amountMsat: 60_000_000, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod})
	if !f.tally(r1) {
		t.Fatal("first receipt not counted")
	}
	if f.tally(r1) {
		t.Fatal("duplicate receipt counted twice")
	}
	f.onEventSaved(context.Background(), r1) // hook path is also idempotent
	if f.raisedMsat != 60_000_000 || f.zapCount != 1 {
		t.Fatalf("raised=%d zaps=%d", f.raisedMsat, f.zapCount)
	}
	if !f.locked() {
		t.Fatal("should still be locked at 60%")
	}

	r2 := makeReceipt(t, providerSK, receiptOpts{amountMsat: 40_000_000, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod + 1})
	f.onEventSaved(context.Background(), r2)
	if f.locked() {
		t.Fatal("should be unlocked once goal is reached")
	}
	s := f.status()
	if !s.Funded || s.Percent != 100 || s.RaisedSats != 100_000 || s.ZapCount != 2 {
		t.Fatalf("status = %+v", s)
	}
	if len(s.Contributors) != 2 || s.Contributors[0].Pubkey != alice.Public().Hex() || s.Contributors[0].Sats != 60_000 {
		t.Fatalf("contributors = %+v", s.Contributors)
	}

	// a receipt that is rejected by validation is never tallied
	bad := makeReceipt(t, nostr.Generate(), receiptOpts{amountMsat: 1_000_000, goalID: f.goal.ID, recipient: f.pk, createdAt: inPeriod})
	if f.tally(bad) {
		t.Fatal("invalid receipt tallied")
	}
}

func TestLockedTruthTable(t *testing.T) {
	// grace period covers "now" (Sept 15) -> writes open even with 0 raised
	f, _ := newTestFunding(t, 100_000, 20)
	if f.locked() {
		t.Fatal("should be open during grace period")
	}
	if !f.status().InGrace {
		t.Fatal("status should report in_grace")
	}

	// grace over, nothing raised -> locked
	f, _ = newTestFunding(t, 100_000, 3)
	if !f.locked() {
		t.Fatal("should be locked after grace with nothing raised")
	}

	// engine not ready (no goal yet) -> fail open
	f.goal = nil
	if f.locked() {
		t.Fatal("should fail open before the goal exists")
	}

	// disabled engine (nil) -> never locked, never a receipt
	var none *funding
	if none.locked() {
		t.Fatal("nil funding must not lock")
	}
	if _, ok := none.isGoalZapReceipt(nostr.Event{Kind: nostr.KindZap}); ok {
		t.Fatal("nil funding must not accept receipts")
	}
	none.onEventSaved(context.Background(), nostr.Event{})
}

func TestOverwriteRelayInformation(t *testing.T) {
	f, providerSK := newTestFunding(t, 100_000, 0)
	req, _ := http.NewRequest(http.MethodGet, "/", nil)

	info := f.overwriteRelayInformation(context.Background(), req, nip11.RelayInformationDocument{Name: "x"})
	if info.Limitation == nil || !info.Limitation.PaymentRequired || !info.Limitation.RestrictedWrites {
		t.Fatalf("locked: limitation = %+v", info.Limitation)
	}
	if info.Fees == nil || len(info.Fees.Subscription) != 1 || info.Fees.Subscription[0].Amount != 100_000_000 || info.Fees.Subscription[0].Unit != "msats" {
		t.Fatalf("locked: fees = %+v", info.Fees)
	}
	if info.PaymentsURL != "https://relay.test/" {
		t.Fatalf("payments_url = %q", info.PaymentsURL)
	}
	if info.Name != "x" {
		t.Fatal("other fields must be preserved")
	}

	// fund it
	r := makeReceipt(t, providerSK, receiptOpts{amountMsat: 100_000_000, goalID: f.goal.ID, recipient: f.pk, createdAt: nostr.Timestamp(f.periodStart.Unix() + 1)})
	f.tally(r)

	info = f.overwriteRelayInformation(context.Background(), req, nip11.RelayInformationDocument{})
	if info.Limitation.PaymentRequired {
		t.Fatal("funded: payment_required should be false")
	}
	if !info.Limitation.RestrictedWrites {
		t.Fatal("restricted_writes must stay true for a WoT relay")
	}
	if info.Fees != nil {
		t.Fatal("funded: fees should be omitted")
	}
	if info.PaymentsURL == "" {
		t.Fatal("payments_url should always be present")
	}

	// disabled engine leaves the document alone
	var none *funding
	orig := nip11.RelayInformationDocument{Name: "y"}
	if got := none.overwriteRelayInformation(context.Background(), req, orig); got.Limitation != nil || got.PaymentsURL != "" {
		t.Fatal("nil funding must not touch relay info")
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[uint64]string{0: "0", 999: "999", 1000: "1,000", 100000: "100,000", 1234567: "1,234,567"} {
		if got := formatSats(in); got != want {
			t.Errorf("formatSats(%d) = %q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"wss://relay.test": "https://relay.test/", "ws://localhost:3334": "http://localhost:3334/", "wss://x/": "https://x/"} {
		if got := websiteURL(in); got != want {
			t.Errorf("websiteURL(%q) = %q want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"a@b.com": "https://b.com/.well-known/lnurlp/a", "x@abc.onion": "http://abc.onion/.well-known/lnurlp/x", "x@localhost:8089": "http://localhost:8089/.well-known/lnurlp/x"} {
		n, d, _ := splitLightningAddress(in)
		if got := lnurlpEndpoint(n, d); got != want {
			t.Errorf("lnurlpEndpoint(%q) = %q want %q", in, got, want)
		}
	}
	if !containsAny([]string{"a", "b"}, []string{"x", "b"}) || containsAny([]string{"a"}, []string{"x"}) {
		t.Error("containsAny")
	}
	if pct(150, 100) != 100 || pct(50, 100) != 50 || pct(1, 0) != 0 {
		t.Error("pct")
	}

	l := newIPLimiter(2, time.Minute)
	if !l.allow("a") || !l.allow("a") || l.allow("a") || !l.allow("b") {
		t.Error("ip limiter")
	}
}

func TestGoalRelaysAndRejectReason(t *testing.T) {
	f, _ := newTestFunding(t, 100_000, 0)
	relays := f.goalRelays()
	if relays[0] != "wss://relay.test" || len(relays) != 3 {
		t.Fatalf("goalRelays = %v", relays)
	}

	// a local/plain-ws relay URL is left out so providers never stall on it
	f.cfg.RelayURL = "ws://localhost:3334"
	relays = f.goalRelays()
	if len(relays) != 2 || relays[0] != "wss://seed.one" {
		t.Fatalf("goalRelays with local url = %v", relays)
	}
	for url, want := range map[string]bool{"wss://relay.test": true, "wss://relay.test:443/": true, "ws://relay.test": false, "wss://localhost:3334": false, "wss://127.0.0.1": false} {
		if publiclyReachable(url) != want {
			t.Errorf("publiclyReachable(%q) = %v", url, !want)
		}
	}
	if msg := f.rejectReason(); msg == "" || msg[:4] != "this" {
		t.Fatalf("rejectReason = %q", msg)
	}
}
