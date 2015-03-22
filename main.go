package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"time"

	"github.com/akrennmair/gouuid"
	"github.com/boltdb/bolt"
	"github.com/julienschmidt/httprouter"
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

	db, err := bolt.Open(*dataFile, 0600, nil)
	if err != nil {
		log.Fatalf("bolt.Open %s failed: %v", *dataFile, err)
	}

	fh := &fileHandler{DB: db, Frontend: *frontend}

	router := httprouter.New()

	router.GET("/:drawer/:file", fh.deliverFile)
	router.DELETE("/:drawer/:file", basicAuth(fh.deleteFile, []byte(*username), []byte(*password)))
	router.POST("/api/upload", basicAuth(fh.uploadFile, []byte(*username), []byte(*password)))

	http.Handle("/", router)

	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

type fileHandler struct {
	DB       *bolt.DB
	Frontend string
}

func (h *fileHandler) deliverFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	ts := time.Now()
	defer func() {
		duration := time.Since(ts)
		log.Printf("deliverying %s took %s", r.RequestURI, duration)
	}()
	drawer := p.ByName("drawer")
	if drawer == "" {
		http.Error(w, "no drawer specified", http.StatusNotFound)
		return
	}

	filename := p.ByName("file")
	if filename == "" {
		http.Error(w, "no filename specified", http.StatusNotFound)
		return
	}

	err := h.DB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(drawer))
		if bucket == nil {
			return errors.New("unknown bucket")
		}

		fileContent := bucket.Get([]byte(filename))
		mimeType := bucket.Get([]byte("." + filename + ".mimetype"))

		w.Header().Set("Content-Type", string(mimeType))
		w.Write(fileContent)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("delivery failed: %v", err)
		return
	}
}

func (h *fileHandler) deleteFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	drawerName := p.ByName("drawer")
	filename := p.ByName("file")

	if drawerName == "" || filename == "" {
		http.Error(w, "Not found", http.StatusNotAcceptable)
	}

	err := h.DB.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(drawerName))
		if bucket == nil {
			return errors.New("unknown drawer")
		}
		if err := bucket.Delete([]byte(filename)); err != nil {
			return err
		}
		if err := bucket.Delete([]byte("." + filename + ".mimetype")); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		http.Error(w, "Deleting file failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *fileHandler) uploadFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parsing multipart form failed: "+err.Error(), http.StatusNotAcceptable)
		return
	}

	drawerName := r.Form.Get("drawer")
	if drawerName == "" {
		http.Error(w, "no drawer name provided", http.StatusNotAcceptable)
		return
	}

	var filenames []string

	err := h.DB.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(drawerName))
		if bucket == nil {
			b, err := tx.CreateBucket([]byte(drawerName))
			if err != nil {
				return err
			}
			bucket = b
		}

		multipartReader, err := r.MultipartReader()
		if err != nil {
			log.Printf("Getting MultipartReader failed: %v", err)
			return err
		}

		for {
			part, err := multipartReader.NextPart()
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}

			uuid := gouuid.New()
			partData, err := ioutil.ReadAll(part)
			if err != nil {
				return err
			}

			filename := uuid.ShortString()
			if extension := r.Form.Get("ext"); extension != "" {
				filename += "." + extension
			}

			if err := bucket.Put([]byte(filename), partData); err != nil {
				return err
			}

			if err := bucket.Put([]byte("."+filename+".mimetype"), []byte(part.Header.Get("Content-Type"))); err != nil {
				return err
			}

			filenames = append(filenames, h.Frontend+"/"+drawerName+"/"+filename)
		}
		return nil
	})
	if err != nil {
		http.Error(w, "upload failed: "+err.Error(), http.StatusInternalServerError)
		log.Printf("upload transaction failed: %v", err)
		return
	}

	if err := json.NewEncoder(w).Encode(filenames); err != nil {
		http.Error(w, "Marshalling JSON failed: "+err.Error(), http.StatusInternalServerError)
		log.Printf("marshalling list of filenames to JSON failed: %v", err)
		return
	}
}
