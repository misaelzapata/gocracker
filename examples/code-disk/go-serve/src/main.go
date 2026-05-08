// go-serve: reads /app/config.json from the code disk.
//   --print   dump the config as JSON and exit (used by smoke tests)
//   --once    start HTTP server, answer one request, exit
//   (no flag) run HTTP server indefinitely
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

type Config struct {
	AppName  string `json:"app_name"`
	Version  string `json:"version"`
	Port     int    `json:"port"`
	Greeting string `json:"greeting"`
}

func main() {
	raw, err := os.ReadFile("/app/config.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read /app/config.json: %v\n", err)
		os.Exit(1)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bad config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Port == 0 {
		cfg.Port = 8080
	}

	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	if mode == "--print" {
		resp := map[string]any{
			"app":      cfg.AppName,
			"version":  cfg.Version,
			"greeting": cfg.Greeting,
			"port":     cfg.Port,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(resp)
		return
	}

	once := mode == "--once"
	done := make(chan struct{})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"app":       cfg.AppName,
			"version":   cfg.Version,
			"greeting":  cfg.Greeting,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		if once {
			close(done)
		}
	})

	addr := ":" + strconv.Itoa(cfg.Port)
	srv := &http.Server{Addr: addr}

	go func() {
		fmt.Fprintf(os.Stdout, "go-serve %s v%s listening on %s\n", cfg.AppName, cfg.Version, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "listen: %v\n", err)
			os.Exit(1)
		}
	}()

	if once {
		<-done
		srv.Close()
	} else {
		select {}
	}
}
