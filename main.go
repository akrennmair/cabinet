package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/akrennmair/gouuid"
	"github.com/boltdb/bolt"
	"github.com/julienschmidt/httprouter"
)

func main() {
	var (
		listenAddr = flag.String("listen", "localhost:8080", "listen address")
		dataFile   = flag.String("datafile", "./data.db", "path to data file")
	)

	flag.Parse()

	db, err := bolt.Open(*dataFile, 0600, nil)
	if err != nil {
		log.Fatalf("bolt.Open %s failed: %v", *dataFile, err)
	}

	fh := &fileHandler{DB: db}

	router := httprouter.New()

	router.GET("/:drawer/:file", fh.deliverFile)
	router.DELETE("/:drawer/:file", fh.deleteFile)
	router.POST("/api/upload", fh.uploadFile)

	log.Fatal(http.ListenAndServe(*listenAddr, router))
}

type fileHandler struct {
	DB *bolt.DB
}

func (h *fileHandler) deliverFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	// TODO: implement
}

func (h *fileHandler) deleteFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	// TODO: implement
}

func (h *fileHandler) uploadFile(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	if err := r.ParseMultipartForm(64 * 1024 * 1024); err != nil {
		http.Error(w, "parsing multipart form failed: "+err.Error(), http.StatusNotAcceptable)
		return
	}

	cabinetName := r.Form.Get("cabinet")
	if cabinetName == "" {
		http.Error(w, "no cabinet name provided", http.StatusNotAcceptable)
		return
	}

	var filenames []string

	err := h.DB.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(cabinetName))
		if bucket == nil {
			return fmt.Errorf("unknown cabinet %s", cabinetName)
		}

		multipartReader, err := r.MultipartReader()
		if err != nil {
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

			filenames = append(filenames, filename)
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
