package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/lmdb"
)

func main() {
	path := flag.String("db", "", "path to the LMDB directory (created if missing)")
	skipErrors := flag.Bool("skip-errors", false, "skip events that fail to parse or save instead of aborting")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: import-jsonl -db <path> [-skip-errors] < events.jsonl")
		os.Exit(2)
	}

	db := &lmdb.LMDBBackend{Path: *path}
	if err := db.Init(); err != nil {
		log.Fatalf("init: %v", err)
	}
	defer db.Close()

	scanner := bufio.NewScanner(os.Stdin)
	// events can exceed the default 64KB line limit
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	var saved, failed uint64
	var line uint64
	for scanner.Scan() {
		line++
		b := scanner.Bytes()
		if len(b) == 0 {
			continue
		}

		var evt nostr.Event
		if err := json.Unmarshal(b, &evt); err != nil {
			failed++
			msg := fmt.Sprintf("line %d: unmarshal: %v", line, err)
			if *skipErrors {
				log.Println(msg)
				continue
			}
			log.Fatal(msg)
		}

		if err := db.SaveEvent(evt); err != nil {
			failed++
			msg := fmt.Sprintf("line %d (id=%s): save: %v", line, evt.ID, err)
			if *skipErrors {
				log.Println(msg)
				continue
			}
			log.Fatal(msg)
		}
		saved++

		if saved%10000 == 0 {
			log.Printf("imported %d events...", saved)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("scan: %v", err)
	}
	log.Printf("done: imported %d events, %d failures, %d lines read", saved, failed, line)
}
