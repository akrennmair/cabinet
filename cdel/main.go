package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
)

func main() {
	var (
		uri  = flag.String("url", "", "file to delete")
		user = flag.String("user", "admin", "username for authentication")
		pass = flag.String("pass", "", "password for authentication")
	)

	flag.Parse()

	if *uri == "" {
		fmt.Println("No URL for deletion provided!")
		flag.Usage()
		return
	}

	req, err := http.NewRequest("DELETE", *uri, nil)
	if err != nil {
		fmt.Printf("Creating request failed: %v\n", err)
		return
	}

	req.SetBasicAuth(*user, *pass)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Request failed: %v\n", err)
		return
	}

	if resp.StatusCode != http.StatusNoContent {
		fmt.Printf("Request failed: HTTP code = %d\n", resp.StatusCode)
		errbody, _ := ioutil.ReadAll(resp.Body)
		fmt.Printf("Additional output: %s\n", string(errbody))
		return
	}

	fmt.Println("OK")
}
