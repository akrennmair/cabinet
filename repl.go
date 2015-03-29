package main

import (
	"fmt"
	"golang.org/x/net/websocket"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akrennmair/cabinet/data"
	"github.com/golang/protobuf/proto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

func dispatchEvents(events <-chan *data.Event, replRequests <-chan replRequest) {
	subscribers := make(map[chan<- *data.Event]struct{})

	for {
		select {
		case e := <-events:
			log.Printf("event: %v", e)
			for ch := range subscribers {
				ch <- e
			}
			log.Printf("finished notifying subscribers")
		case r := <-replRequests:
			switch r.Type {
			case Subscribe:
				subscribers[r.Events] = struct{}{}
			case Unsubscribe:
				delete(subscribers, r.Events)
			}
		}
	}
}

type replicator struct {
	ParentServer string
	DB           *leveldb.DB
	Username     string
	Password     string
}

func (r *replicator) replicate() {
	count := 0
	for {
		ts := time.Now()
		err := r.replicateUntilError()
		if err != nil {
			log.Printf("Replication error: %v", err)
			if time.Since(ts) < 5*time.Second {
				if count < 5 {
					count++
				}
			} else {
				count = 0
			}
			if count > 0 {
				backoffTime := time.Duration(math.Pow(2, float64(count))) * time.Second
				log.Printf("Backing off for %s", backoffTime)
				time.Sleep(backoffTime)
			}
		}
	}
}

func (r *replicator) replicateUntilError() error {
	uri := r.ParentServer + "/api/repl"
	parsedURL, err := url.Parse(uri)
	if err != nil {
		log.Printf("parsing %s failed: %v", uri, err)
		return err
	}
	switch parsedURL.Scheme {
	case "http":
		parsedURL.Scheme = "ws"
	case "https":
		parsedURL.Scheme = "wss"
	default:
		return fmt.Errorf("unknown URL scheme %q", parsedURL.Scheme)
	}

	cfg, err := websocket.NewConfig(parsedURL.String(), "http://localhost/")
	if err != nil {
		log.Printf("creating websocket config failed: %v", err)
		return err
	}
	cfg.Location = parsedURL
	cfg.Header.Set("Authorization", "Basic "+basicAuthEncode(r.Username, r.Password))

	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		log.Printf("connecting to %s failed: %v", parsedURL, err)
		return err
	}
	defer ws.Close()

	latestEvent, err := r.DB.Get([]byte("latest_event"), nil)
	if err != nil {
		latestEvent = []byte("event:0")
	}

	var replStart data.ReplicationStart
	replStart.Event = proto.String(string(latestEvent))

	rawReplStartMsg, err := proto.Marshal(&replStart)
	if err != nil {
		log.Printf("marshalling replication start message failed: %v", err)
		return err
	}

	if err := websocket.Message.Send(ws, rawReplStartMsg); err != nil {
		log.Printf("sending replication start message failed: %v", err)
		return err
	}

	for {
		var rawMsg []byte
		if err := websocket.Message.Receive(ws, &rawMsg); err != nil {
			log.Printf("receiving event failed: %v", err)
			return err
		}

		var event data.Event
		if err := proto.Unmarshal(rawMsg, &event); err != nil {
			log.Printf("unmarshalling event failed: %v", err)
			return err
		}

		if haveEvent, _ := r.DB.Has([]byte(event.GetId()), nil); haveEvent {
			log.Printf("ignoring duplicate event %s", event.GetId())
			continue
		}

		batch := new(leveldb.Batch)
		batch.Put([]byte(event.GetId()), rawMsg)
		batch.Put([]byte("latest_event"), []byte(event.GetId()))

		switch event.GetType() {
		case data.Event_UPLOAD:
			fileContent, mimeType, err := r.downloadFile(r.ParentServer + "/" + event.GetDrawer() + "/" + event.GetFilename())
			if err != nil {
				log.Printf("Error downloading %s:%s, ignoring file: %v", event.GetDrawer(), event.GetFilename(), err)
			} else {
				batch.Put([]byte("file:"+event.GetDrawer()+":"+event.GetFilename()), fileContent)

				var metadata data.MetaData
				metadata.ContentType = proto.String(mimeType)
				rawMetaData, err := proto.Marshal(&metadata)
				if err != nil {
					log.Printf("marshalling meta data failed: %v", err)
					return err
				}

				batch.Put([]byte("meta:"+event.GetDrawer()+":"+event.GetFilename()), rawMetaData)
			}
		case data.Event_DELETE:
			batch.Delete([]byte("file:" + event.GetDrawer() + ":" + event.GetFilename()))
		default:
			return fmt.Errorf("unknown event type %d", event.GetType())
		}

		if err := r.DB.Write(batch, nil); err != nil {
			log.Printf("writing replicated event to database failed: %v", err)
			return err
		}

		log.Printf("replicated %s to %s:%s", event.GetId(), event.GetDrawer(), event.GetFilename())
	}

	return nil
}

func (r *replicator) downloadFile(uri string) (content []byte, contentType string, err error) {
	resp, err := http.Get(uri)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s returned %d", uri, resp.StatusCode)
	}
	content, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	contentType = resp.Header.Get("Content-Type")
	return content, contentType, nil
}

type replHandler struct {
	DB         *leveldb.DB
	Username   string
	Password   string
	Replicator chan<- replRequest
}

type replRequest struct {
	Type   replRequestType
	Events chan<- *data.Event
}

type replRequestType int

const (
	Subscribe replRequestType = iota
	Unsubscribe
)

func (h *replHandler) handleWebsocket(conn *websocket.Conn) {
	if !basicAuth(nil, conn.Request(), h.Username, h.Password) {
		return
	}

	var msg []byte

	if err := websocket.Message.Receive(conn, &msg); err != nil {
		log.Printf("Receiving from replication websocket failed: %v", err)
		return
	}

	var replStart data.ReplicationStart
	if err := proto.Unmarshal(msg, &replStart); err != nil {
		log.Printf("Decoding ReplicationStart failed: %v", err)
		return
	}

	if !strings.HasPrefix(replStart.GetEvent(), "event:") {
		log.Printf("Got invalid event: %s", replStart.GetEvent())
		return
	}

	/*
		this whole replication code works like this:

		We first register to receive events to replicate them to this connected child.

		We then start a goroutine to cache incoming events (later referred to as
		"caching goroutine") until we're finished with catching
		up missing events.

		We then start another goroutine ("forwarding goroutine") that first catches
		up with missing events by iterating through all events since the last event
		the child got. We then switch over to forwarding events that have been
		cached by the caching goroutine.

		We then start a third goroutine ("receiving goroutine") that waits for the
		websocket to receive a message (which will never happen) or for the
		connection to close. If the connection is closed, the receivin goroutine
		closes the quit channel to indicate that replication work needs to be
		stopped.

		The handleWebsocket method waits for the quit signal, and then returns. Due
		to a defer, the channel through which we receive events gets unregistered,
		and the rawEvents channel gets closed, as well. This makes the forwarding
		goroutine end its operation. The quit signal also ends the caching goroutine.

	*/
	events := make(chan *data.Event, 1)
	rawEvents := make(chan []byte)

	defer close(rawEvents)

	h.Replicator <- replRequest{Type: Subscribe, Events: events}

	defer func() {
		h.Replicator <- replRequest{Type: Unsubscribe, Events: events}
		close(events)
	}()

	quit := make(chan bool)

	go cacheEvents(events, rawEvents, quit)

	go func() {
		iterator := h.DB.NewIterator(&util.Range{Start: []byte(replStart.GetEvent()), Limit: []byte("f")}, nil)

		for iterator.Next() {
			eventData := iterator.Value()
			if err := websocket.Message.Send(conn, eventData); err != nil {
				log.Printf("Sending event failed: %v", err)
				return
			}
		}

		for event := range rawEvents {
			if err := websocket.Message.Send(conn, event); err != nil {
				log.Printf("Sending event failed: %v", err)
				return
			}
		}

		log.Printf("stopped sending events to client")
	}()

	go func() {
		defer close(quit)
		for {
			var inbuf []byte
			if err := websocket.Message.Receive(conn, &inbuf); err != nil {
				log.Printf("client has disconnected: %v", err)
				return
			}
		}
	}()

	<-quit
	log.Printf("handleWebsocket: received signal to stop replicating to client")
}

func cacheEvents(incomingEvents <-chan *data.Event, outgoingEvents chan<- []byte, quit <-chan bool) {
	cachedEvents := [][]byte{}

	for {
		// if there are currently no cached events, only attempt to receive and listen for
		// the quit signal.
		if len(cachedEvents) == 0 {
			select {
			case e := <-incomingEvents:
				rawData, err := proto.Marshal(e)
				if err != nil {
					return
				}
				cachedEvents = append(cachedEvents, rawData)
			case <-quit:
				return
			}
		} else {
			// if there are cached events, attempt to receive, send and listen for the quit
			// signal.
			select {
			case e := <-incomingEvents:
				rawData, err := proto.Marshal(e)
				if err != nil {
					return
				}
				cachedEvents = append(cachedEvents, rawData)
			case outgoingEvents <- cachedEvents[0]:
				cachedEvents = cachedEvents[1:]
			case <-quit:
				return
			}
		}
	}
}
