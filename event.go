package main

import (
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/boltdb/bolt"
)

type event struct {
	Type   eventType
	Drawer string
	File   string
}

type eventType int

const (
	eventBucketName = "_events"

	uploadFileEvent eventType = iota
	deleteFileEvent
)

func isValidDrawerName(drawerName string) bool {
	return drawerName != "" && drawerName != eventBucketName
}

func logEvents(events <-chan event, db *bolt.DB) {
	err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(eventBucketName))
		return err
	})
	if err != nil {
		log.Printf("couldn't create bucket %s: %v", eventBucketName, err)
	}

	for event := range events {
		eventKey := strconv.FormatInt(time.Now().UnixNano(), 10)
		eventData, err := json.Marshal(event)
		if err != nil {
			log.Printf("json.Marshal failed: %v", err)
			continue
		}

		err = db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket([]byte(eventBucketName))
			if err := bucket.Put([]byte(eventKey), eventData); err != nil {
				return err
			}
			log.Printf("event: %s -> %s", eventKey, eventData)
			return nil
		})
		if err != nil {
			log.Printf("couldn't store event %#v: %v", event, err)
		}
	}
}
