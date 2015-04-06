package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

func TestFileHandler(t *testing.T) {
	db, err := leveldb.Open(storage.NewMemStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}

	uploadHandler := &uploadFileHandler{DB: db, Frontend: "http://localhost:8080", AuthFunc: authFunc}
	fileHandler := &fileHandler{DB: db, AuthFunc: authFunc}

	// first, upload file.
	response := httptest.NewRecorder()

	var multipartData bytes.Buffer

	mw := multipart.NewWriter(&multipartData)
	headers := make(textproto.MIMEHeader)

	contentType := "application/x-test-type"

	headers.Set("Content-Type", contentType)

	pw, err := mw.CreatePart(headers)
	if err != nil {
		t.Fatal(err)
	}

	pw.Write([]byte("hello world!"))
	mw.Close()

	t.Logf("generated multipart data: %s", multipartData.String())

	request, err := http.NewRequest("POST", "/api/upload?ext=foo&drawer=test", &multipartData)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", mw.FormDataContentType())
	request.Header.Set("Authorization", "Basic "+basicAuthEncode("dummy", "auth"))

	uploadHandler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d instead.", response.Code)
	}

	filenames := []string{}

	if err := json.NewDecoder(response.Body).Decode(&filenames); err != nil {
		t.Fatalf("couldn't decode upload response: %v", err)
	}

	if len(filenames) != 1 {
		t.Fatalf("expected one filename, got %d.", len(filenames))
	}

	// then download the same file.
	fileRequest, err := http.NewRequest("GET", filenames[0], nil)
	if err != nil {
		t.Fatal(err)
	}

	fileResponse := httptest.NewRecorder()

	fileHandler.ServeHTTP(fileResponse, fileRequest)

	if fileResponse.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d instead.", fileResponse.Code)
	}

	if ct := fileResponse.Header().Get("Content-Type"); ct != contentType {
		t.Fatalf("expected Content-Type %s, got %s instead.", contentType, ct)
	}

	if fileResponse.Body.String() != "hello world!" {
		t.Fatalf("expected \"hello world!\", got HTTP response body: %q", fileResponse.Body.String())
	}

	// then delete the same file.
	deleteRequest, err := http.NewRequest("DELETE", filenames[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteRequest.Header.Set("Authorization", "Basic "+basicAuthEncode("dummy", "auth"))

	deleteResponse := httptest.NewRecorder()

	fileHandler.ServeHTTP(deleteResponse, deleteRequest)

	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d instead.", deleteResponse.Code)
	}

	// then download the same file again, this should fail.
	secondFileResponse := httptest.NewRecorder()

	fileHandler.ServeHTTP(secondFileResponse, fileRequest)

	if secondFileResponse.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d intead.", secondFileResponse.Code)
	}

	// start up test server that delivers a text file.
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "test data")
	}))

	// test store API endpoint.
	storeRequest, err := http.NewRequest("GET", "/api/store?url="+testServer.URL+"/test.txt&drawer=test", nil)
	if err != nil {
		t.Fatal(err)
	}
	storeRequest.Header.Set("Authorization", "Basic "+basicAuthEncode("dummy", "auth"))
	storeResponse := httptest.NewRecorder()

	uploadHandler.ServeHTTP(storeResponse, storeRequest)

	if storeResponse.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d instead.", storeResponse.Code)
	}

	storeFileRequest, err := http.NewRequest("GET", storeResponse.Body.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// then query file uploaded via /api/store.
	storeFileResponse := httptest.NewRecorder()
	fileHandler.ServeHTTP(storeFileResponse, storeFileRequest)
	if storeFileResponse.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d instead.", storeFileResponse.Code)
	}

	if ct := storeFileResponse.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %s.", ct)
	}

	if body := storeFileResponse.Body.String(); body != "test data" {
		t.Fatalf("expected \"test data\", got %q.", body)
	}

	expectedContentLocation := testServer.URL + "/test.txt"

	if cl := storeFileResponse.Header().Get("Content-Location"); cl != expectedContentLocation {
		t.Fatalf("expected Content-Location %s, got %s.", expectedContentLocation, cl)
	}
}

func authFunc(username, password string) bool {
	return true
}
