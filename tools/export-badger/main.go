// Standalone tool: dumps every event from an old wot-relay Badger DB to
// JSONL on stdout. Uses the legacy github.com/fiatjaf/eventstore/badger +
// github.com/nbd-wtf/go-nostr packages so it can read the pre-migration
// on-disk format. Has its own go.mod to keep those deps out of the main
// module. Pipe the output into cmd/import-jsonl to populate the new
// LMDB store.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/fiatjaf/eventstore/badger"
	"github.com/nbd-wtf/go-nostr"
)

func main() {
	path := flag.String("db", "", "path to the Badger DB directory")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: export-badger -db <path> > events.jsonl")
		os.Exit(2)
	}

	db := badger.BadgerBackend{Path: *path}
	if err := db.Init(); err != nil {
		log.Fatalf("init: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	ch, err := db.QueryEvents(ctx, nostr.Filter{})
	if err != nil {
		log.Fatalf("query: %v", err)
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	var n uint64
	for evt := range ch {
		if err := enc.Encode(evt); err != nil {
			log.Fatalf("encode event %s: %v", evt.ID, err)
		}
		n++
		if n%10000 == 0 {
			log.Printf("exported %d events...", n)
		}
	}
	log.Printf("done: exported %d events", n)
}
