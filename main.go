package main

import (
	"encoding/json"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"strconv"
	"time"

	"github.com/akrennmair/cabinet/data"
	"github.com/akrennmair/gouuid"
	"github.com/golang/protobuf/proto"
	"github.com/julienschmidt/httprouter"
	"github.com/syndtr/goleveldb/leveldb"
)

func main() {
	var (
		listenAddr = flag.String("listen", "localhost:8080", "listen address")
		dataFile   = flag.String("datafile", "./data.db", "path to data file")
		username   = flag.String("user", "admin", "user name for operations requiring authentication")
		password   = flag.String("pass", "", "password for operations requiring authentication")
		frontend   = flag.String("frontend", "", "front-facing URL for the file delivery")
	)

	flag.Parse()

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

	//go logEvents(events, db)

	fh := &fileHandler{DB: db, Frontend: *frontend, Events: events}

	router := httprouter.New()

	router.GET("/:drawer/:file", fh.deliverFile)
	router.DELETE("/:drawer/:file", basicAuth(fh.deleteFile, []byte(*username), []byte(*password)))
	router.POST("/api/upload", basicAuth(fh.uploadFile, []byte(*username), []byte(*password)))

	http.Handle("/", router)

	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

type fileHandler struct {
	DB       *leveldb.DB
	Frontend string
	Events   chan<- *data.Event
}

func (h *fileHandler) deliverFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	ts := time.Now()
	defer func() {
		duration := time.Since(ts)
		log.Printf("delivering %s took %s", r.RequestURI, duration)
	}()
	drawer := p.ByName("drawer")
	if drawer == "" {
		http.Error(w, "no valid drawer specified", http.StatusNotFound)
		return
	}

	filename := p.ByName("file")
	if filename == "" {
		http.Error(w, "no filename specified", http.StatusNotFound)
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

func (h *fileHandler) deleteFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	drawerName := p.ByName("drawer")
	filename := p.ByName("file")

	if drawerName == "" || filename == "" {
		http.Error(w, "Not found", http.StatusNotAcceptable)
		return
	}

	batch := new(leveldb.Batch)
	batch.Delete([]byte("file:" + drawerName + ":" + filename))
	batch.Delete([]byte("meta:" + drawerName + ":" + filename))

	event := &data.Event{
		Type:     data.Event_DELETE.Enum(),
		Drawer:   proto.String(drawerName),
		Filename: proto.String(filename),
	}

	eventData, err := proto.Marshal(event)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	batch.Put([]byte("event:"+strconv.FormatInt(time.Now().UnixNano(), 10)), eventData)

	if err := h.DB.Write(batch, nil); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		log.Printf("deleting file %s:%s failed: %v", drawerName, filename, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.Events <- event
}

func (h *fileHandler) uploadFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
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

		event := &data.Event{
			Type:     data.Event_DELETE.Enum(),
			Drawer:   proto.String(drawerName),
			Filename: proto.String(filename),
		}

		eventData, err := proto.Marshal(event)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		batch.Put([]byte("event:"+strconv.FormatInt(time.Now().UnixNano(), 10)), eventData)

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
