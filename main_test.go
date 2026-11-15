package main

import (
	"slices"
	"testing"

	"fiatjaf.com/nostr"
)

func TestCheckWoTEvent(t *testing.T) {
	origBooted := booted.Load()
	origFallback := fallbackAcceptAll.Load()
	trustNetworkMutex.Lock()
	origMap := trustNetworkMap
	trustNetworkMutex.Unlock()

	t.Cleanup(func() {
		booted.Store(origBooted)
		fallbackAcceptAll.Store(origFallback)
		trustNetworkMutex.Lock()
		trustNetworkMap = origMap
		trustNetworkMutex.Unlock()
	})

	trustedPK := nostr.Generate().Public()
	untrustedPK := nostr.Generate().Public()

	trustNetworkMutex.Lock()
	trustNetworkMap = map[string]bool{trustedPK.Hex(): true}
	trustNetworkMutex.Unlock()

	booted.Store(false)
	fallbackAcceptAll.Store(false)
	if reject, reason := checkWoTEvent(nostr.Event{PubKey: trustedPK, Kind: nostr.KindTextNote}); !reject || reason != "trust network is still being built" {
		t.Fatalf("pre-boot expected reject for build window, got reject=%v reason=%q", reject, reason)
	}

	booted.Store(true)
	if reject, reason := checkWoTEvent(nostr.Event{PubKey: trustedPK, Kind: nostr.KindTextNote}); reject {
		t.Fatalf("booted trusted note rejected: %q", reason)
	}
	if reject, reason := checkWoTEvent(nostr.Event{PubKey: trustedPK, Kind: nostr.KindEncryptedDirectMessage}); !reject || reason != "only gift wrapped DMs are allowed" {
		t.Fatalf("expected legacy DM rejection, got reject=%v reason=%q", reject, reason)
	}
	if reject, reason := checkWoTEvent(nostr.Event{PubKey: untrustedPK, Kind: nostr.KindTextNote}); !reject || reason != "not in web of trust" {
		t.Fatalf("expected untrusted rejection, got reject=%v reason=%q", reject, reason)
	}

	// Boot deadline fallback accepts events while network is empty, then recovers.
	trustNetworkMutex.Lock()
	trustNetworkMap = map[string]bool{}
	trustNetworkMutex.Unlock()
	fallbackAcceptAll.Store(true)
	if reject, reason := checkWoTEvent(nostr.Event{PubKey: untrustedPK, Kind: nostr.KindTextNote}); reject {
		t.Fatalf("fallback accept-all rejected event: %q", reason)
	}

	fallbackAcceptAll.Store(false)
	if reject, reason := checkWoTEvent(nostr.Event{PubKey: untrustedPK, Kind: nostr.KindTextNote}); !reject || reason != "not in web of trust" {
		t.Fatalf("post-recovery expected untrusted rejection, got reject=%v reason=%q", reject, reason)
	}
}

func TestOwnerWriteRelaysFromEvents(t *testing.T) {
	if got := ownerWriteRelaysFromEvents(nil); len(got) != 0 {
		t.Fatalf("expected empty relays for nil events, got %v", got)
	}

	older := nostr.Event{
		CreatedAt: 100,
		Kind:      nostr.KindRelayListMetadata,
		Tags: nostr.Tags{
			{"r", "wss://old.example.com", "write"},
		},
	}
	newer := nostr.Event{
		CreatedAt: 200,
		Kind:      nostr.KindRelayListMetadata,
		Tags: nostr.Tags{
			{"r", "wss://read-only.example.com", "read"},
			{"r", "wss://write.example.com", "write"},
			{"r", "wss://both.example.com"},
			{"r", "wss://write.example.com/", "write"},
		},
	}

	got := ownerWriteRelaysFromEvents([]nostr.Event{older, newer})
	want := []string{"wss://write.example.com", "wss://both.example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
