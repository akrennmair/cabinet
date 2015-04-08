package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/akrennmair/cabinet/basicauth"
	"github.com/akrennmair/cabinet/data"
	"github.com/akrennmair/gouuid"
	"github.com/golang/protobuf/proto"
	"github.com/syndtr/goleveldb/leveldb"
	"golang.org/x/net/websocket"
)

func main() {
	var (
		listenAddr  = flag.String("listen", "localhost:8080", "listen address")
		dataFile    = flag.String("datafile", "./data.db", "path to data file")
		username    = flag.String("user", "admin", "user name for operations requiring authentication")
		password    = flag.String("pass", "", "password for operations requiring authentication")
		frontend    = flag.String("frontend", "", "front-facing URL for the file delivery")
		parent      = flag.String("parent", "", "parent server URL, e.g. http://otherserver:8080")
		forceParent = flag.Bool("forceparent", false, "if enabled, forces instance to act as a parent even though it replicates from another parent server")
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

	expvar.Publish("leveldb.stats", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.stats"); return stats }))
	expvar.Publish("leveldb.sstables", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.sstables"); return stats }))
	expvar.Publish("leveldb.blockpool", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.blockpool"); return stats }))
	expvar.Publish("leveldb.cachedblock", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.cachedblock"); return stats }))
	expvar.Publish("leveldb.openedtables", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.openedtables"); return stats }))
	expvar.Publish("leveldb.alivesnaps", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.alivesnaps"); return stats }))
	expvar.Publish("leveldb.aliveiters", expvar.Func(func() interface{} { stats, _ := db.GetProperty("leveldb.aliveiters"); return stats }))

	events := make(chan *data.Event)

	// start replication from parent server when in child mode.
	if *parent != "" {
		log.Printf("Starting replication from %s", *parent)
		r := replicator{ParentServer: *parent, DB: db, Username: *username, Password: *password, Events: events}
		go r.replicate()
	}

	replRequests := make(chan replRequest)

	go dispatchEvents(events, replRequests)

	authFunc := func(u, p string) bool {
		return u == *username && p == *password
	}

	// only enable upload when in parent mode.
	if *parent == "" || *forceParent {
		uploadHandler := &uploadFileHandler{DB: db, Frontend: *frontend, Events: events, AuthFunc: authFunc}
		http.Handle("/api/upload", uploadHandler)
		http.Handle("/api/store", uploadHandler)
	}
	repl := &replHandler{DB: db, AuthFunc: authFunc, Replicator: replRequests}
	http.Handle("/api/repl", websocket.Handler(repl.handleWebsocket))
	http.Handle("/", &fileHandler{DB: db, Events: events, AuthFunc: authFunc, ChildMode: (*parent != "" && !*forceParent)})

	mux := basicauth.NewHandler(http.DefaultServeMux, authFunc, []string{"/debug/vars"})

	log.Fatal(http.ListenAndServe(*listenAddr, mux))
}

type fileHandler struct {
	DB        *leveldb.DB
	Events    chan<- *data.Event
	ChildMode bool
	AuthFunc  basicauth.AuthenticatorFunc
}

var (
	deleteCount  = expvar.NewInt("cabinet.delete.count")
	deliverCount = expvar.NewInt("cabinet.deliver.count")
	uploadCount  = expvar.NewInt("cabinet.upload.count")
)

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
	if metadata.Source != nil {
		w.Header().Set("Content-Location", metadata.GetSource())
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(fileContent)), 10))
	if _, err := w.Write(fileContent); err != nil {
		log.Printf("delivery of %s:%s failed: %v", drawer, filename, err)
	}

	deliverCount.Add(1)
}

func (h *fileHandler) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !basicauth.Authenticate(w, r, h.AuthFunc) {
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
	if h.Events != nil {
		h.Events <- event
	}

	deleteCount.Add(1)
}

type uploadFileHandler struct {
	DB       *leveldb.DB
	Frontend string
	Events   chan<- *data.Event
	AuthFunc basicauth.AuthenticatorFunc
}

func (h *uploadFileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	if !basicauth.Authenticate(w, r, h.AuthFunc) {
		return
	}

	pathFields := strings.Split(r.URL.Path, "/")
	apiEndpoint := pathFields[len(pathFields)-1]

	switch apiEndpoint {
	case "upload":
		if r.Method != "POST" {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		h.upload(w, r)
	case "store":
		if r.Method != "GET" {
			log.Printf("Method = %s", r.Method)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		h.store(w, r)
	default:
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
	}
}

func (h *uploadFileHandler) store(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parsing form failed: "+err.Error(), http.StatusNotAcceptable)
		return
	}

	uri := r.FormValue("url")
	if uri == "" {
		http.Error(w, "empty url parameter", http.StatusNotAcceptable)
		return
	}
	parsedURI, err := url.Parse(uri)
	if err != nil {
		http.Error(w, "invalid URL: "+err.Error(), http.StatusNotAcceptable)
		return
	}

	drawerName := r.FormValue("drawer")
	if drawerName == "" || !validDrawerName(drawerName) {
		http.Error(w, "invalid drawer name", http.StatusNotAcceptable)
		return
	}

	var buf bytes.Buffer
	resp, err := http.Get(uri)
	if err != nil {
		http.Error(w, "Fetching URL failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if _, err := io.Copy(&buf, resp.Body); err != nil {
		http.Error(w, "Reading HTTP body failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	filename := gouuid.New().ShortString()
	if extension := r.Form.Get("ext"); extension != "" {
		filename += "." + extension
	} else if n := strings.LastIndex(parsedURI.Path, "."); n != -1 {
		filename += parsedURI.Path[n:]
	}

	batch := new(leveldb.Batch)

	var metadata data.MetaData
	metadata.ContentType = proto.String(resp.Header.Get("Content-Type"))
	metadata.Source = proto.String(uri)
	rawMetaData, err := proto.Marshal(&metadata)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("proto.Marshal failed: %v", err)
		return
	}

	batch.Put([]byte("file:"+drawerName+":"+filename), buf.Bytes())
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

	if err := h.DB.Write(batch, nil); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("store transaction failed: %v", err)
		return
	}

	fmt.Fprintf(w, "%s/%s/%s", h.Frontend, drawerName, filename)
}

func (h *uploadFileHandler) upload(w http.ResponseWriter, r *http.Request) {
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
	if drawerName == "" || !validDrawerName(drawerName) {
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

	if h.Events != nil {
		for _, event := range events {
			h.Events <- event
		}
	}

	uploadCount.Add(1)
}

func basicAuthEncode(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func validDrawerName(drawer string) bool {
	for _, r := range drawer {
		if !strings.ContainsRune("abcefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789..:,;$-", r) {
			return false
		}
	}
	return true
}
