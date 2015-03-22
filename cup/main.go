package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path"
	"strings"
	"time"
)

func main() {
	var (
		destinationAddr = flag.String("dest", "http://localhost:8080", "destination instance to which the file shall be uploaded")
		mimeType        = flag.String("mimetype", "application/octet-stream", "MIME type of file")
		inputFile       = flag.String("file", "", "file to upload")
		drawerName      = flag.String("drawer", "", "drawer name")
		auth            = flag.String("auth", "", "authentication information, provided as username:password")
	)

	flag.Parse()

	if *inputFile == "" {
		fmt.Println("No input file provided!")
		flag.Usage()
		return
	}

	if *drawerName == "" {
		fmt.Println("No drawer name provided!")
		flag.Usage()
		return
	}

	f, err := os.Open(*inputFile)
	if err != nil {
		fmt.Printf("Error: couldn't open %s: %v\n", *inputFile, err)
		return
	}

	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	mw.SetBoundary("cabinet-upload-" + time.Now().Format("20060102150405999999999"))

	mimeHeaders := make(textproto.MIMEHeader)
	mimeHeaders.Set("Content-Type", *mimeType)

	pw, err := mw.CreatePart(mimeHeaders)
	if err != nil {
		fmt.Printf("Error: couldn't create multipart data: %v\n", err)
		return
	}

	if _, err := io.Copy(pw, f); err != nil {
		fmt.Printf("Error: couldn't add file content to multipart data: %v\n", err)
		return
	}

	if err := mw.Close(); err != nil {
		fmt.Printf("Error: couldn't finish up multipart data: %v\n", err)
		return
	}

	basename := path.Base(*inputFile)
	baseParts := strings.Split(basename, ".")
	extension := baseParts[len(baseParts)-1]

	req, err := http.NewRequest("POST", *destinationAddr+"/api/upload?drawer="+*drawerName+"&ext="+extension, &mpBuf)
	if err != nil {
		fmt.Printf("Error: couldn't create request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if *auth != "" {
		elems := strings.Split(*auth, ":")
		if len(elems) != 2 {
			fmt.Println("Error: authentication information must be in the format username:password!")
			return
		}
		req.SetBasicAuth(elems[0], elems[1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error: upload failed: %v\n", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Print("Error: upload failed: ")
		io.Copy(os.Stdout, resp.Body)
		return
	}

	filenames := []string{}

	if err := json.NewDecoder(resp.Body).Decode(&filenames); err != nil {
		fmt.Printf("Decoding response failed: %v\n", err)
		return
	}

	for _, fn := range filenames {
		fmt.Println(fn)
	}
}
