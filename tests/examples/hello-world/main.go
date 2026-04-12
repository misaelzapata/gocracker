package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	if err := run(":8080", os.Stdout); err != nil {
		panic(err)
	}
}

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello from gocracker!")
	})
	return mux
}

func run(addr string, stdout io.Writer) error {
	fmt.Fprintln(stdout, "Listening on", addr)
	return http.ListenAndServe(addr, newMux())
}
