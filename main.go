package main

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/lmdb"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/policies"
	"fiatjaf.com/nostr/nip11"
	"github.com/joho/godotenv"
)

var (
	version string
)

type Config struct {
	RelayName        string
	RelayPubkey      string
	RelayDescription string
	DBPath           string
	RelayURL         string
	IndexPath        string
	StaticPath       string
	RefreshInterval  int
	MinimumFollowers int
	ArchivalSync     bool
	RelayContact     string
	RelayIcon        string
	MaxAgeDays       int
	ArchiveReactions bool
	IgnoredPubkeys   []string
	MaxTrustNetwork  int
	MaxRelays        int
	MaxOneHopNetwork int
	SeedRelays       []string
	ArchiveKinds     []nostr.Kind
}

var defaultSeedRelays = []string{
	"wss://nos.lol",
	"wss://nostr.mom",
	"wss://purplepag.es",
	"wss://purplerelay.com",
	"wss://relay.damus.io",
	"wss://relay.nostr.band",
	"wss://relay.snort.social",
	"wss://relayable.org",
	"wss://relay.primal.net",
	"wss://relay.nostr.bg",
	"wss://no.str.cr",
	"wss://nostr21.com",
	"wss://nostrue.com",
	"wss://relay.siamstr.com",
}

var defaultArchiveKinds = []nostr.Kind{
	nostr.KindArticle,
	nostr.KindDeletion,
	nostr.KindFollowList,
	nostr.KindEncryptedDirectMessage,
	nostr.KindMuteList,
	nostr.KindRelayListMetadata,
	nostr.KindRepost,
	nostr.KindZapRequest,
	nostr.KindZap,
	nostr.KindTextNote,
}

var pool *nostr.Pool
var db *lmdb.LMDBBackend
var relays []string
var relaySet = make(map[string]bool) // O(1) lookup
var config Config
var trustNetwork []string
var trustNetworkSet = make(map[string]bool) // O(1) lookup
var seedRelays []string
var booted bool
var oneHopNetwork []string
var oneHopNetworkSet = make(map[string]bool) // O(1) lookup
var trustNetworkMap map[string]bool
var pubkeyFollowerCount = make(map[string]int)
var trustedNotes uint64
var untrustedNotes uint64
var archiveEventSemaphore = make(chan struct{}, 20)

// Performance counters
var (
	totalEvents         uint64
	rejectedEvents      uint64
	archivedEvents      uint64
	profileRefreshCount uint64
	networkRefreshCount uint64
)

// Mutexes for thread safety
var (
	relayMutex        sync.RWMutex
	trustNetworkMutex sync.RWMutex
	oneHopMutex       sync.RWMutex
	followerMutex     sync.RWMutex
)

func main() {
	nostr.InfoLogger = log.New(io.Discard, "", 0)
	booted = false
	green := "\033[32m"
	reset := "\033[0m"

	art := `
888       888      88888888888      8888888b.          888
888   o   888          888          888   Y88b         888
888  d8b  888          888          888    888         888
888 d888b 888  .d88b.  888          888   d88P .d88b.  888  8888b.  888  888
888d88888b888 d88""88b 888          8888888P" d8P  Y8b 888     "88b 888  888
88888P Y88888 888  888 888          888 T88b  88888888 888 .d888888 888  888
8888P   Y8888 Y88..88P 888          888  T88b Y8b.     888 888  888 Y88b 888
888P     Y888  "Y88P"  888          888   T88b "Y8888  888 "Y888888  "Y88888
                                                                         888
                                                                    Y8b d88P
                                               powered by: khatru     "Y88P"
	`

	fmt.Println(green + art + reset)
	log.Println("🚀 booting up web of trust relay")
	relay := khatru.NewRelay()
	ctx := context.Background()
	pool = nostr.NewPool()
	pool.Context = ctx
	config = LoadConfig()

	relay.Info = &nip11.RelayInformationDocument{
		Name:        config.RelayName,
		Description: config.RelayDescription,
		Contact:     config.RelayContact,
		Icon:        config.RelayIcon,
		Software:    "https://github.com/bitvora/wot-relay",
		Version:     version,
	}
	if pk, err := nostr.PubKeyFromHex(config.RelayPubkey); err == nil {
		relay.Info.PubKey = &pk
	}

	appendPubkey(config.RelayPubkey)

	db = &lmdb.LMDBBackend{Path: config.DBPath}
	if err := db.Init(); err != nil {
		panic(err)
	}
	relay.UseEventstore(db, 500)

	relay.OnEvent = policies.SeqEvent(
		policies.RejectEventsWithBase64Media,
		policies.EventIPRateLimiter(5, time.Minute*1, 30),
		func(ctx context.Context, event nostr.Event) (bool, string) {
			atomic.AddUint64(&totalEvents, 1)

			pkHex := event.PubKey.Hex()
			trustNetworkMutex.RLock()
			trusted := trustNetworkMap[pkHex]
			hasNetwork := len(trustNetworkMap) > 0
			trustNetworkMutex.RUnlock()

			// While the trust network is still being built for the first time (no
			// list yet), accept everything. On subsequent rebuilds the old map
			// remains active via atomic swap, so we never reject against an empty
			// list.
			if !hasNetwork {
				return false, ""
			}

			if !trusted {
				atomic.AddUint64(&rejectedEvents, 1)
				return true, "not in web of trust"
			}
			if event.Kind == nostr.KindEncryptedDirectMessage {
				atomic.AddUint64(&rejectedEvents, 1)
				return true, "only gift wrapped DMs are allowed"
			}

			return false, ""
		},
	)

	relay.OnRequest = policies.SeqRequest(
		policies.NoEmptyFilters,
		policies.NoComplexFilters,
		policies.FilterIPRateLimiter(5, time.Minute*1, 30),
	)

	relay.RejectConnection = policies.ConnectionRateLimiter(10, time.Minute*2, 30)

	seedRelays = config.SeedRelays

	go refreshTrustNetwork(ctx, relay)
	go monitorMemoryUsage()
	go monitorPerformance()

	mux := relay.Router()
	static := http.FileServer(http.Dir(config.StaticPath))

	mux.Handle("GET /static/", http.StripPrefix("/static/", static))
	mux.Handle("GET /favicon.ico", http.StripPrefix("/", static))

	mux.HandleFunc("GET /debug/stats", debugStatsHandler)
	mux.HandleFunc("GET /debug/goroutines", debugGoroutinesHandler)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.ParseFiles(os.Getenv("INDEX_PATH")))
		data := struct {
			RelayName        string
			RelayPubkey      string
			RelayDescription string
			RelayURL         string
		}{
			RelayName:        config.RelayName,
			RelayPubkey:      config.RelayPubkey,
			RelayDescription: config.RelayDescription,
			RelayURL:         config.RelayURL,
		}
		err := tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3334"
	}
	log.Printf("🎉 relay running on port :%s", port)
	log.Println("🔍 debug endpoints available at:")
	log.Printf("   http://localhost:%s/debug/pprof/ (CPU/memory profiling)", port)
	log.Printf("   http://localhost:%s/debug/stats (application stats)", port)
	log.Printf("   http://localhost:%s/debug/goroutines (goroutine info)", port)
	err := http.ListenAndServe(":"+port, relay)
	if err != nil {
		log.Fatal(err)
	}
}

func LoadConfig() Config {
	godotenv.Load(".env")

	if os.Getenv("REFRESH_INTERVAL_HOURS") == "" {
		os.Setenv("REFRESH_INTERVAL_HOURS", "3")
	}

	refreshInterval, _ := strconv.Atoi(os.Getenv("REFRESH_INTERVAL_HOURS"))
	log.Println("🔄 refresh interval set to", refreshInterval, "hours")

	if os.Getenv("MINIMUM_FOLLOWERS") == "" {
		os.Setenv("MINIMUM_FOLLOWERS", "1")
	}

	if os.Getenv("ARCHIVAL_SYNC") == "" {
		os.Setenv("ARCHIVAL_SYNC", "TRUE")
	}

	if os.Getenv("RELAY_ICON") == "" {
		os.Setenv("RELAY_ICON", "https://pfp.nostr.build/56306a93a88d4c657d8a3dfa57b55a4ed65b709eee927b5dafaab4d5330db21f.png")
	}

	if os.Getenv("RELAY_CONTACT") == "" {
		os.Setenv("RELAY_CONTACT", getEnv("RELAY_PUBKEY"))
	}

	if os.Getenv("MAX_AGE_DAYS") == "" {
		os.Setenv("MAX_AGE_DAYS", "0")
	}

	if os.Getenv("ARCHIVE_REACTIONS") == "" {
		os.Setenv("ARCHIVE_REACTIONS", "FALSE")
	}

	if os.Getenv("MAX_TRUST_NETWORK") == "" {
		os.Setenv("MAX_TRUST_NETWORK", "40000")
	}

	if os.Getenv("MAX_RELAYS") == "" {
		os.Setenv("MAX_RELAYS", "1000")
	}

	if os.Getenv("MAX_ONE_HOP_NETWORK") == "" {
		os.Setenv("MAX_ONE_HOP_NETWORK", "50000")
	}

	ignoredPubkeys := []string{}
	if ignoreList := os.Getenv("IGNORE_FOLLOWS_LIST"); ignoreList != "" {
		ignoredPubkeys = splitAndTrim(ignoreList)
	}

	minimumFollowers, _ := strconv.Atoi(os.Getenv("MINIMUM_FOLLOWERS"))
	maxAgeDays, _ := strconv.Atoi(os.Getenv("MAX_AGE_DAYS"))
	maxTrustNetwork, _ := strconv.Atoi(os.Getenv("MAX_TRUST_NETWORK"))
	maxRelays, _ := strconv.Atoi(os.Getenv("MAX_RELAYS"))
	maxOneHopNetwork, _ := strconv.Atoi(os.Getenv("MAX_ONE_HOP_NETWORK"))

	// Parse configurable seed relays, fall back to defaults
	parsedSeedRelays := defaultSeedRelays
	if sr := os.Getenv("SEED_RELAYS"); sr != "" {
		parsedSeedRelays = splitAndTrim(sr)
		log.Printf("🌱 using %d custom seed relays", len(parsedSeedRelays))
	} else {
		log.Printf("🌱 using %d default seed relays", len(parsedSeedRelays))
	}

	// Parse configurable archive kinds, fall back to defaults
	archiveReactions := getEnv("ARCHIVE_REACTIONS") == "TRUE"
	parsedArchiveKinds := make([]nostr.Kind, len(defaultArchiveKinds))
	copy(parsedArchiveKinds, defaultArchiveKinds)
	if ak := os.Getenv("ARCHIVE_KINDS"); ak != "" {
		kindStrs := splitAndTrim(ak)
		parsedArchiveKinds = []nostr.Kind{}
		for _, ks := range kindStrs {
			if k, err := strconv.Atoi(ks); err == nil {
				parsedArchiveKinds = append(parsedArchiveKinds, nostr.Kind(k))
			} else {
				log.Printf("⚠️ ignoring invalid ARCHIVE_KINDS value: %s", ks)
			}
		}
		log.Printf("📦 using %d custom archive kinds", len(parsedArchiveKinds))
	}
	// Add reaction kind if ARCHIVE_REACTIONS is enabled and not already present
	if archiveReactions {
		hasReaction := false
		for _, k := range parsedArchiveKinds {
			if k == nostr.KindReaction {
				hasReaction = true
				break
			}
		}
		if !hasReaction {
			parsedArchiveKinds = append(parsedArchiveKinds, nostr.KindReaction)
		}
	}

	config := Config{
		RelayName:        getEnv("RELAY_NAME"),
		RelayPubkey:      getEnv("RELAY_PUBKEY"),
		RelayDescription: getEnv("RELAY_DESCRIPTION"),
		RelayContact:     getEnv("RELAY_CONTACT"),
		RelayIcon:        getEnv("RELAY_ICON"),
		DBPath:           getEnv("DB_PATH"),
		RelayURL:         getEnv("RELAY_URL"),
		IndexPath:        getEnv("INDEX_PATH"),
		StaticPath:       getEnv("STATIC_PATH"),
		RefreshInterval:  refreshInterval,
		MinimumFollowers: minimumFollowers,
		ArchivalSync:     getEnv("ARCHIVAL_SYNC") == "TRUE",
		MaxAgeDays:       maxAgeDays,
		ArchiveReactions: archiveReactions,
		IgnoredPubkeys:   ignoredPubkeys,
		MaxTrustNetwork:  maxTrustNetwork,
		MaxRelays:        maxRelays,
		MaxOneHopNetwork: maxOneHopNetwork,
		SeedRelays:       parsedSeedRelays,
		ArchiveKinds:     parsedArchiveKinds,
	}

	return config
}

func getEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		log.Fatalf("Environment variable %s not set", key)
	}
	return value
}

func updateTrustNetworkFilter() {
	// Build new trust network in temporary variables
	newTrustNetworkMap := make(map[string]bool)
	var newTrustNetwork []string
	newTrustNetworkSet := make(map[string]bool)

	log.Println("🌐 building new trust network map")

	followerMutex.RLock()
	for pubkey, count := range pubkeyFollowerCount {
		if count >= config.MinimumFollowers {
			newTrustNetworkMap[pubkey] = true
			if !newTrustNetworkSet[pubkey] && len(pubkey) == 64 && len(newTrustNetwork) < config.MaxTrustNetwork {
				newTrustNetwork = append(newTrustNetwork, pubkey)
				newTrustNetworkSet[pubkey] = true
			}
		}
	}
	followerMutex.RUnlock()

	// Now atomically replace the active trust network
	trustNetworkMutex.Lock()
	trustNetworkMap = newTrustNetworkMap
	trustNetwork = newTrustNetwork
	trustNetworkSet = newTrustNetworkSet
	trustNetworkMutex.Unlock()

	log.Println("🌐 trust network map updated with", len(newTrustNetwork), "keys")

	// Cleanup follower count map periodically to prevent unbounded growth
	followerMutex.Lock()
	if len(pubkeyFollowerCount) > config.MaxOneHopNetwork*2 {
		log.Println("🧹 cleaning follower count map")
		newFollowerCount := make(map[string]int)
		for pubkey, count := range pubkeyFollowerCount {
			if count >= config.MinimumFollowers || newTrustNetworkMap[pubkey] {
				newFollowerCount[pubkey] = count
			}
		}
		oldCount := len(pubkeyFollowerCount)
		pubkeyFollowerCount = newFollowerCount
		log.Printf("🧹 cleaned follower count map: %d -> %d entries", oldCount, len(newFollowerCount))
	}
	followerMutex.Unlock()
}

func hexToPubKeys(hexes []string) []nostr.PubKey {
	out := make([]nostr.PubKey, 0, len(hexes))
	for _, h := range hexes {
		if pk, err := nostr.PubKeyFromHex(h); err == nil {
			out = append(out, pk)
		}
	}
	return out
}

func refreshProfiles(ctx context.Context) {
	atomic.AddUint64(&profileRefreshCount, 1)
	start := time.Now()

	// Get a snapshot of current trust network to avoid holding locks during network operations
	trustNetworkMutex.RLock()
	currentTrustNetwork := make([]string, len(trustNetwork))
	copy(currentTrustNetwork, trustNetwork)
	trustNetworkMutex.RUnlock()

	const (
		profileBatchSize    = 500
		profileConcurrency  = 5
		profileBatchTimeout = 30 * time.Second
	)

	sem := make(chan struct{}, profileConcurrency)
	var wg sync.WaitGroup

	for i := 0; i < len(currentTrustNetwork); i += profileBatchSize {
		end := i + profileBatchSize
		if end > len(currentTrustNetwork) {
			end = len(currentTrustNetwork)
		}
		batch := currentTrustNetwork[i:end]

		wg.Add(1)
		sem <- struct{}{}
		go func(batch []string) {
			defer wg.Done()
			defer func() { <-sem }()

			timeout, cancel := context.WithTimeout(ctx, profileBatchTimeout)
			defer cancel()

			filter := nostr.Filter{
				Authors: hexToPubKeys(batch),
				Kinds:   []nostr.Kind{nostr.KindProfileMetadata},
			}

			for ev := range pool.FetchMany(timeout, seedRelays, filter, nostr.SubscriptionOptions{}) {
				if err := db.SaveEvent(ev.Event); err != nil {
					nostr.InfoLogger.Printf("save profile: %v", err)
				}
			}
		}(batch)
	}
	wg.Wait()
	duration := time.Since(start)
	log.Printf("👤 profiles refreshed: %d profiles in %v", len(currentTrustNetwork), duration)
}

func refreshTrustNetwork(ctx context.Context, relay *khatru.Relay) {
	runTrustNetworkRefresh := func() {
		atomic.AddUint64(&networkRefreshCount, 1)
		start := time.Now()

		// Build new networks in temporary variables to avoid disrupting the active network
		var newOneHopNetwork []string
		newOneHopNetworkSet := make(map[string]bool)
		newPubkeyFollowerCount := make(map[string]int)

		// Copy existing follower counts to preserve data
		followerMutex.RLock()
		for k, v := range pubkeyFollowerCount {
			newPubkeyFollowerCount[k] = v
		}
		followerMutex.RUnlock()

		timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		ownerPk, err := nostr.PubKeyFromHex(config.RelayPubkey)
		if err != nil {
			log.Printf("invalid RELAY_PUBKEY: %v", err)
			return
		}
		filter := nostr.Filter{
			Authors: []nostr.PubKey{ownerPk},
			Kinds:   []nostr.Kind{nostr.KindFollowList},
		}

		log.Println("🔍 fetching owner's follows")
		eventCount := 0
		for ev := range pool.FetchMany(timeoutCtx, seedRelays, filter, nostr.SubscriptionOptions{}) {
			eventCount++
			for contact := range ev.Tags.FindAll("p") {
				if len(contact) < 2 {
					continue
				}
				pubkey := contact[1]
				if isIgnored(pubkey, config.IgnoredPubkeys) {
					fmt.Println("ignoring follows from pubkey: ", pubkey)
					continue
				}
				newPubkeyFollowerCount[pubkey]++

				// Add to new one-hop network
				if !newOneHopNetworkSet[pubkey] && len(pubkey) == 64 && len(newOneHopNetwork) < config.MaxOneHopNetwork {
					newOneHopNetwork = append(newOneHopNetwork, pubkey)
					newOneHopNetworkSet[pubkey] = true
				}
			}
		}
		log.Printf("🔍 processed %d follow list events", eventCount)

		log.Println("🌐 building web of trust graph")
		var totalProcessed int64
		var followerCountMu sync.Mutex

		const (
			wotBatchSize    = 500
			wotConcurrency  = 5
			wotBatchTimeout = 30 * time.Second
		)

		sem := make(chan struct{}, wotConcurrency)
		var wg sync.WaitGroup
		var completedBatches int64
		totalBatches := (len(newOneHopNetwork) + wotBatchSize - 1) / wotBatchSize

		for i := 0; i < len(newOneHopNetwork); i += wotBatchSize {
			end := i + wotBatchSize
			if end > len(newOneHopNetwork) {
				end = len(newOneHopNetwork)
			}
			batch := newOneHopNetwork[i:end]

			wg.Add(1)
			sem <- struct{}{}
			go func(batch []string) {
				defer wg.Done()
				defer func() { <-sem }()

				timeout, cancel := context.WithTimeout(ctx, wotBatchTimeout)
				defer cancel()

				filter := nostr.Filter{
					Authors: hexToPubKeys(batch),
					Kinds:   []nostr.Kind{nostr.KindFollowList, nostr.KindRelayListMetadata, nostr.KindProfileMetadata},
				}

				for ev := range pool.FetchMany(timeout, seedRelays, filter, nostr.SubscriptionOptions{}) {
					atomic.AddInt64(&totalProcessed, 1)

					hasP := false
					followerCountMu.Lock()
					for contact := range ev.Tags.FindAll("p") {
						if len(contact) > 1 {
							newPubkeyFollowerCount[contact[1]]++
							hasP = true
						}
					}
					followerCountMu.Unlock()
					_ = hasP

					for relayTag := range ev.Tags.FindAll("r") {
						if len(relayTag) > 1 {
							appendRelay(relayTag[1])
						}
					}

					if ev.Kind == nostr.KindProfileMetadata {
						if err := db.SaveEvent(ev.Event); err != nil {
							nostr.InfoLogger.Printf("save profile: %v", err)
						}
					}
				}

				done := atomic.AddInt64(&completedBatches, 1)
				if done%10 == 0 || done == int64(totalBatches) {
					log.Printf("🌐 wot graph: %d/%d batches complete (%d events so far)",
						done, totalBatches, atomic.LoadInt64(&totalProcessed))
				}
			}(batch)
		}
		wg.Wait()

		// Now atomically replace the active data structures
		oneHopMutex.Lock()
		oneHopNetwork = newOneHopNetwork
		oneHopNetworkSet = newOneHopNetworkSet
		oneHopMutex.Unlock()

		followerMutex.Lock()
		pubkeyFollowerCount = newPubkeyFollowerCount
		followerMutex.Unlock()

		duration := time.Since(start)
		log.Printf("🫂 total network size: %d (processed %d events in %v)", len(newPubkeyFollowerCount), atomic.LoadInt64(&totalProcessed), duration)
		relayMutex.RLock()
		log.Println("🔗 relays discovered:", len(relays))
		relayMutex.RUnlock()
	}

	ticker := time.NewTicker(time.Duration(config.RefreshInterval) * time.Hour)
	defer ticker.Stop()

	// Run initial refresh
	log.Println("🚀 performing initial trust network build...")
	runTrustNetworkRefresh()
	updateTrustNetworkFilter()

	// Mark as booted after initial trust network is built
	booted = true
	log.Println("✅ trust network initialized, relay is now active")

	deleteOldNotes()
	archiveTrustedNotes(ctx, relay)

	// Then run on timer
	for {
		select {
		case <-ticker.C:
			log.Println("🔄 refreshing trust network in background...")
			runTrustNetworkRefresh()
			updateTrustNetworkFilter()
			deleteOldNotes()
			archiveTrustedNotes(ctx, relay)
			log.Println("✅ trust network refresh completed")
		case <-ctx.Done():
			return
		}
	}
}

func appendRelay(relay string) {
	relayMutex.Lock()
	defer relayMutex.Unlock()

	if len(relays) >= config.MaxRelays {
		return // Prevent unbounded growth
	}

	if relaySet[relay] {
		return // Already exists
	}

	relays = append(relays, relay)
	relaySet[relay] = true
}

func appendPubkey(pubkey string) {
	trustNetworkMutex.Lock()
	defer trustNetworkMutex.Unlock()

	if len(trustNetwork) >= config.MaxTrustNetwork {
		return // Prevent unbounded growth
	}

	if trustNetworkSet[pubkey] {
		return // Already exists
	}

	if len(pubkey) != 64 {
		return
	}

	trustNetwork = append(trustNetwork, pubkey)
	trustNetworkSet[pubkey] = true
}

func archiveTrustedNotes(ctx context.Context, relay *khatru.Relay) {
	timeout, cancel := context.WithTimeout(ctx, time.Duration(config.RefreshInterval)*time.Hour)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)
		if config.ArchivalSync {
			go refreshProfiles(ctx)

			since := nostr.Now()
			filter := nostr.Filter{
				Kinds: config.ArchiveKinds,
				Since: since,
			}

			log.Println("📦 archiving trusted notes...")

			eventCount := 0
			for ev := range pool.SubscribeMany(timeout, seedRelays, filter, nostr.SubscriptionOptions{}) {
				eventCount++

				// Check GC pressure every 1000 events
				if eventCount%1000 == 0 {
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					if m.NumGC > 0 && eventCount > 1000 {
						gcRate := float64(m.NumGC) / float64(eventCount/1000)
						if gcRate > 2.0 {
							log.Printf("⚠️ High GC pressure (%.1f GC/1000 events), slowing archive process", gcRate)
							time.Sleep(100 * time.Millisecond)
						}
					}
				}

				// Use semaphore to limit concurrent goroutines
				select {
				case archiveEventSemaphore <- struct{}{}:
					go func(event nostr.Event) {
						defer func() { <-archiveEventSemaphore }()
						archiveEvent(ctx, relay, event)
					}(ev.Event)
				case <-timeout.Done():
					log.Printf("📦 archive timeout reached, processed %d events", eventCount)
					return
				default:
					archiveEvent(ctx, relay, ev.Event)
				}
			}

			log.Printf("📦 archived %d trusted notes and discarded %d untrusted notes (processed %d total events)",
				atomic.LoadUint64(&trustedNotes), atomic.LoadUint64(&untrustedNotes), eventCount)
		} else {
			log.Println("🔄 web of trust will refresh in", config.RefreshInterval, "hours")
			<-timeout.Done()
		}
	}()

	select {
	case <-timeout.Done():
		log.Println("restarting process")
	case <-done:
		log.Println("📦 archiving process completed")
	}
}

func archiveEvent(ctx context.Context, relay *khatru.Relay, ev nostr.Event) {
	trustNetworkMutex.RLock()
	trusted := trustNetworkMap[ev.PubKey.Hex()]
	trustNetworkMutex.RUnlock()

	if trusted {
		if err := db.SaveEvent(ev); err != nil {
			nostr.InfoLogger.Printf("save event: %v", err)
		}
		relay.BroadcastEvent(ev)
		atomic.AddUint64(&trustedNotes, 1)
		atomic.AddUint64(&archivedEvents, 1)
	} else {
		atomic.AddUint64(&untrustedNotes, 1)
	}
}

func deleteOldNotes() error {
	if config.MaxAgeDays <= 0 {
		log.Printf("MAX_AGE_DAYS disabled")
		return nil
	}

	maxAgeSecs := nostr.Timestamp(config.MaxAgeDays * 86400)
	oldAge := nostr.Now() - maxAgeSecs
	if oldAge <= 0 {
		log.Printf("MAX_AGE_DAYS too large")
		return nil
	}

	filter := nostr.Filter{
		Until: oldAge,
		Kinds: config.ArchiveKinds,
		Limit: 1000,
	}

	count := 0
	for evt := range db.QueryEvents(filter, 1000) {
		if err := db.DeleteEvent(evt.ID); err != nil {
			log.Printf("error deleting note %s: %v", evt.ID, err)
			return err
		}
		count++
	}

	if count == 0 {
		log.Println("0 old notes found")
	} else {
		log.Printf("%d old (until %d) notes deleted", count, oldAge)
	}

	return nil
}

func splitAndTrim(input string) []string {
	items := strings.Split(input, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
	}
	return items
}

func isIgnored(pubkey string, ignoredPubkeys []string) bool {
	for _, ignored := range ignoredPubkeys {
		if pubkey == ignored {
			return true
		}
	}
	return false
}

// Add memory monitoring
func monitorMemoryUsage() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		relayMutex.RLock()
		relayCount := len(relays)
		relayMutex.RUnlock()

		trustNetworkMutex.RLock()
		trustNetworkCount := len(trustNetwork)
		trustNetworkMutex.RUnlock()

		oneHopMutex.RLock()
		oneHopCount := len(oneHopNetwork)
		oneHopMutex.RUnlock()

		followerMutex.RLock()
		followerCount := len(pubkeyFollowerCount)
		followerMutex.RUnlock()

		log.Printf("📊 Memory: Alloc=%d KB, Sys=%d KB, NumGC=%d",
			m.Alloc/1024, m.Sys/1024, m.NumGC)
		log.Printf("📊 Data structures: Relays=%d, TrustNetwork=%d, OneHop=%d, Followers=%d",
			relayCount, trustNetworkCount, oneHopCount, followerCount)
	}
}

// Add performance monitoring
func monitorPerformance() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	var lastGC uint32
	var lastEvents, lastRejected, lastArchived uint64

	for range ticker.C {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		currentEvents := atomic.LoadUint64(&totalEvents)
		currentRejected := atomic.LoadUint64(&rejectedEvents)
		currentArchived := atomic.LoadUint64(&archivedEvents)

		eventsPerMin := currentEvents - lastEvents
		rejectedPerMin := currentRejected - lastRejected
		archivedPerMin := currentArchived - lastArchived
		gcPerMin := m.NumGC - lastGC

		numGoroutines := runtime.NumGoroutine()

		log.Printf("⚡ Performance: Events/min=%d, Rejected/min=%d, Archived/min=%d, GC/min=%d, Goroutines=%d",
			eventsPerMin, rejectedPerMin, archivedPerMin, gcPerMin, numGoroutines)

		if gcPerMin > 60 {
			log.Printf("⚠️  HIGH GC ACTIVITY: %d garbage collections in last minute!", gcPerMin)
		}

		if numGoroutines > 1000 {
			log.Printf("⚠️  HIGH GOROUTINE COUNT: %d goroutines active!", numGoroutines)
		}

		lastGC = m.NumGC
		lastEvents = currentEvents
		lastRejected = currentRejected
		lastArchived = currentArchived
	}
}

// Debug handlers
func debugStatsHandler(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	stats := fmt.Sprintf(`Debug Statistics:

Memory:
  Allocated: %d KB
  System: %d KB
  Total Allocations: %d
  GC Cycles: %d
  Goroutines: %d

Events:
  Total Events: %d
  Rejected Events: %d
  Archived Events: %d
  Trusted Notes: %d
  Untrusted Notes: %d

Refreshes:
  Profile Refreshes: %d
  Network Refreshes: %d

Data Structures:
  Relays: %d
  Trust Network: %d
  One Hop Network: %d
  Follower Count Map: %d
`,
		m.Alloc/1024,
		m.Sys/1024,
		m.Mallocs,
		m.NumGC,
		runtime.NumGoroutine(),
		atomic.LoadUint64(&totalEvents),
		atomic.LoadUint64(&rejectedEvents),
		atomic.LoadUint64(&archivedEvents),
		atomic.LoadUint64(&trustedNotes),
		atomic.LoadUint64(&untrustedNotes),
		atomic.LoadUint64(&profileRefreshCount),
		atomic.LoadUint64(&networkRefreshCount),
		len(relays),
		len(trustNetwork),
		len(oneHopNetwork),
		len(pubkeyFollowerCount),
	)

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(stats))
}

func debugGoroutinesHandler(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 1<<20)
	stackSize := runtime.Stack(buf, true)

	w.Header().Set("Content-Type", "text/plain")
	w.Write(buf[:stackSize])
}
