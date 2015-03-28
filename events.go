package main

import (
	"log"

	"github.com/akrennmair/cabinet/data"
)

func dispatchEvents(events <-chan *data.Event) {
	for e := range events {
		// TODO: implement.
		log.Printf("event: %v", e)
	}
}
