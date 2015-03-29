package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akrennmair/cabinet/data"
	"github.com/akrennmair/gouuid"
	"github.com/golang/protobuf/proto"
	"github.com/syndtr/goleveldb/leveldb"
	"golang.org/x/net/websocket"
)

func main() {
	var (
		listenAddr = flag.String("listen", "localhost:8080", "listen address")
		dataFile   = flag.String("datafile", "./data.db", "path to data file")
		username   = flag.String("user", "admin", "user name for operations requiring authentication")
		password   = flag.String("pass", "", "password for operations requiring authentication")
		frontend   = flag.String("frontend", "", "front-facing URL for the file delivery")
		parent     = flag.String("parent", "", "parent server URL, e.g. http://otherserver:8080")
	)

	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if *username == "" || *password == "" {
		log.Fatal("You need to provide username and password!")
	}

	if *frontend == "" {
		log.Fatal("You need to provide a front-facing URL, e.g. http://localhost:8080")
	}

	if _, err := url.Parse(*frontend); err != nil {
		log.Fatalf("Invalid front-facing URL: %v", err)
	}

	db, err := leveldb.OpenFile(*dataFile, nil)
	if err != nil {
		log.Fatalf("leveldb.OpenFile %s failed: %v", *dataFile, err)
	}

	events := make(chan *data.Event)

	// start replication from parent server when in child mode.
	if *parent != "" {
		r := replicator{ParentServer: *parent, DB: db, Username: *username, Password: *password}
		go r.replicate()
	}

	replRequests := make(chan replRequest)

	go dispatchEvents(events, replRequests)

	// only enable upload when in parent mode.
	if *parent == "" {
		http.Handle("/api/upload", &uploadFileHandler{DB: db, Frontend: *frontend, Events: events, Username: *username, Password: *password})
	}
	repl := &replHandler{DB: db, Username: *username, Password: *password, Replicator: replRequests}
	http.Handle("/api/repl", websocket.Handler(repl.handleWebsocket))
	http.Handle("/", &fileHandler{DB: db, Events: events, Username: *username, Password: *password, ChildMode: *parent != ""})

	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

type fileHandler struct {
	DB        *leveldb.DB
	Events    chan<- *data.Event
	Username  string
	Password  string
	ChildMode bool
}

func (h *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "DELETE":
		if h.ChildMode {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.deleteFile(w, r)
	case "GET":
		h.deliverFile(w, r)
	default:
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
	}
}

func (h *fileHandler) deliverFile(w http.ResponseWriter, r *http.Request) {
	ts := time.Now()
	defer func() {
		duration := time.Since(ts)
		log.Printf("delivering %s took %s", r.RequestURI, duration)
	}()

	uriParts := strings.Split(r.URL.Path[1:], "/")
	if len(uriParts) != 2 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	drawer, filename := uriParts[0], uriParts[1]
	if drawer == "" || filename == "" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	fileContent, err := h.DB.Get([]byte("file:"+drawer+":"+filename), nil)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusNoContent), http.StatusNotFound)
		return
	}

	var metadata data.MetaData

	rawMetaData, err := h.DB.Get([]byte("meta:"+drawer+":"+filename), nil)
	if err != nil {
		log.Printf("couldn't find metadata for %s:%s: %v", drawer, filename, err)
		metadata.ContentType = proto.String("application/octet-stream")
	} else {
		if err := proto.Unmarshal(rawMetaData, &metadata); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			log.Printf("proto.Unmarshal of metadata for %s:%s failed: %v", drawer, filename, err)
			return
		}
	}

	w.Header().Set("Content-Type", metadata.GetContentType())
	if _, err := w.Write(fileContent); err != nil {
		log.Printf("delivery of %s:%s failed: %v", drawer, filename, err)
	}
}

func (h *fileHandler) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !basicAuth(w, r, h.Username, h.Password) {
		return
	}

	uriParts := strings.Split(r.URL.Path[1:], "/")
	if len(uriParts) != 2 {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	drawerName := uriParts[0]
	if drawerName == "" {
		http.Error(w, "no valid drawer specified", http.StatusNotFound)
		return
	}

	filename := uriParts[1]
	if filename == "" {
		http.Error(w, "no filename specified", http.StatusNotFound)
		return
	}

	batch := new(leveldb.Batch)
	batch.Delete([]byte("file:" + drawerName + ":" + filename))
	batch.Delete([]byte("meta:" + drawerName + ":" + filename))

	eventKey := "event:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	event := &data.Event{
		Type:     data.Event_DELETE.Enum(),
		Drawer:   proto.String(drawerName),
		Filename: proto.String(filename),
		Id:       proto.String(eventKey),
	}

	eventData, err := proto.Marshal(event)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	batch.Put([]byte(eventKey), eventData)
	batch.Put([]byte("latest_event"), []byte(eventKey))

	if err := h.DB.Write(batch, nil); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("deleting file %s:%s failed: %v", drawerName, filename, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.Events <- event
}

type uploadFileHandler struct {
	DB       *leveldb.DB
	Frontend string
	Events   chan<- *data.Event
	Username string
	Password string
}

func (h *uploadFileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	if !basicAuth(w, r, h.Username, h.Password) {
		return
	}

	ts := time.Now()
	defer func() {
		duration := time.Since(ts)
		log.Printf("upload took %s", duration)
	}()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "parsing multipart form failed: "+err.Error(), http.StatusNotAcceptable)
		return
	}

	drawerName := r.Form.Get("drawer")
	if drawerName == "" {
		http.Error(w, "no valid drawer name provided", http.StatusNotAcceptable)
		return
	}

	var filenames []string

	var events []*data.Event

	batch := new(leveldb.Batch)

	multipartReader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("Getting MultipartReader failed: %v", err)
		return
	}

	for {
		part, err := multipartReader.NextPart()
		if err != nil {
			if err == io.EOF {
				break
			}
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		uuid := gouuid.New()
		partData, err := ioutil.ReadAll(part)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		filename := uuid.ShortString()
		if extension := r.Form.Get("ext"); extension != "" {
			filename += "." + extension
		}
		batch.Put([]byte("file:"+drawerName+":"+filename), partData)

		var metadata data.MetaData
		metadata.ContentType = proto.String(part.Header.Get("Content-Type"))
		rawMetaData, err := proto.Marshal(&metadata)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			log.Printf("proto.Marshal failed: %v", err)
			return
		}

		batch.Put([]byte("meta:"+drawerName+":"+filename), rawMetaData)

		eventKey := "event:" + strconv.FormatInt(time.Now().UnixNano(), 10)
		event := &data.Event{
			Type:     data.Event_UPLOAD.Enum(),
			Drawer:   proto.String(drawerName),
			Filename: proto.String(filename),
			Id:       proto.String(eventKey),
		}

		eventData, err := proto.Marshal(event)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		batch.Put([]byte(eventKey), eventData)
		batch.Put([]byte("latest_event"), []byte(eventKey))

		filenames = append(filenames, h.Frontend+"/"+drawerName+"/"+filename)
		events = append(events, event)
	}

	if err := h.DB.Write(batch, nil); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("upload transaction failed: %v", err)
		return
	}

	if err := json.NewEncoder(w).Encode(filenames); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("marshalling list of filenames to JSON failed: %v", err)
		return
	}

	for _, event := range events {
		h.Events <- event
	}
}

func basicAuth(w http.ResponseWriter, r *http.Request, user, pass string) bool {
	const basicAuthPrefix string = "Basic "

	// Get the Basic Authentication credentials
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, basicAuthPrefix) {
		// Check credentials
		payload, err := base64.StdEncoding.DecodeString(auth[len(basicAuthPrefix):])
		if err == nil {
			pair := bytes.SplitN(payload, []byte(":"), 2)
			if len(pair) == 2 && bytes.Equal(pair[0], []byte(user)) && bytes.Equal(pair[1], []byte(pass)) {
				// Delegate request to the given handle
				return true
			}
		}
	}

	if w != nil {
		// Request Basic Authentication otherwise
		w.Header().Set("WWW-Authenticate", "Basic realm=Restricted")
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	}
	return false
}

func basicAuthEncode(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
