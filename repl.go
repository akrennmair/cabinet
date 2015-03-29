package main

import (
	"fmt"
	"golang.org/x/net/websocket"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"

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
			// TODO: this is a very naive implementation that may block up everything.
			for ch := range subscribers {
				ch <- e
			}
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
	for {
		err := r.replicateUntilError()
		if err != nil {
			log.Printf("Replication error: %v", err)
		}
		// TODO: implement backing-off strategy here.
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

	if _, err := ws.Write(rawReplStartMsg); err != nil {
		log.Printf("sending replication start message failed: %v", err)
		return err
	}

	for {
		var inbuf [2048]byte
		n, err := ws.Read(inbuf[:])
		if err != nil {
			log.Printf("reading event failed: %v", err)
			return err
		}
		rawMsg := inbuf[:n]

		var event data.Event
		if err := proto.Unmarshal(rawMsg, &event); err != nil {
			log.Printf("unmarshalling event failed: %v", err)
			return err
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

	var inbuf [4096]byte

	n, err := conn.Read(inbuf[:])
	if err != nil {
		log.Printf("Reading from replication websocket failed: %v", err)
		return
	}

	msg := inbuf[:n]

	var replStart data.ReplicationStart
	if err := proto.Unmarshal(msg, &replStart); err != nil {
		log.Printf("Decoding ReplicationStart failed: %v", err)
		return
	}

	if !strings.HasPrefix(replStart.GetEvent(), "event:") {
		log.Printf("Got invalid event: %s", replStart.GetEvent())
		return
	}

	iterator := h.DB.NewIterator(&util.Range{Start: []byte(replStart.GetEvent()), Limit: []byte("f")}, nil)

	for iterator.Next() {
		eventData := iterator.Value()
		if _, err := conn.Write(eventData); err != nil {
			log.Printf("Sending event failed: %v", err)
			return
		}
	}

	events := make(chan *data.Event)

	// TODO: this is an absolutely naive implementation. events between the last
	// event from the iterator and this replication request get lost. This needs
	// to be fixed.
	h.Replicator <- replRequest{Type: Subscribe, Events: events}

	defer func() {
		h.Replicator <- replRequest{Type: Unsubscribe, Events: events}
	}()

	// TODO: how does this code deal with a disconnecting child?
	for event := range events {
		eventData, err := proto.Marshal(event)
		if err != nil {
			log.Printf("Marshalling event failed: %v", err)
			return
		}
		if _, err := conn.Write(eventData); err != nil {
			log.Printf("Sending event failed: %v", err)
			return
		}
	}
}
