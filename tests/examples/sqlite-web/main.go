// Tiny HTTP service backed by SQLite. Uses modernc.org/sqlite (pure Go,
// no cgo) so the resulting binary runs from a scratch image.
//
//   POST /notes  body=text/plain  -> {"id": N}
//   GET  /notes                    -> [{"id":1,"body":"..."}, ...]
//   GET  /notes/{id}               -> {"id":N,"body":"..."}
//   GET  /health                   -> {"status":"ok","count":N}
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

type note struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

func main() {
	dbPath := os.Getenv("SQLITE_PATH")
	if dbPath == "" {
		dbPath = "/data/notes.db"
	}
	if err := os.MkdirAll("/data", 0o755); err != nil {
		log.Fatalf("mkdir /data: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS notes (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		body TEXT NOT NULL
	)`); err != nil {
		log.Fatalf("create table: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		var n int64
		_ = db.QueryRow("SELECT COUNT(*) FROM notes").Scan(&n)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "count": n})
	})

	mux.HandleFunc("/notes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) == 0 {
				http.Error(w, "empty body", http.StatusBadRequest)
				return
			}
			res, err := db.Exec("INSERT INTO notes(body) VALUES(?)", string(body))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			id, _ := res.LastInsertId()
			writeJSON(w, http.StatusCreated, map[string]any{"id": id})
		case http.MethodGet:
			rows, err := db.Query("SELECT id, body FROM notes ORDER BY id")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer rows.Close()
			var out []note
			for rows.Next() {
				var n note
				if err := rows.Scan(&n.ID, &n.Body); err == nil {
					out = append(out, n)
				}
			}
			writeJSON(w, http.StatusOK, out)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/notes/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/notes/")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		var n note
		if err := db.QueryRow("SELECT id, body FROM notes WHERE id=?", id).Scan(&n.ID, &n.Body); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, n)
	})

	addr := ":8080"
	if v := os.Getenv("PORT"); v != "" {
		addr = ":" + v
	}
	log.Printf("sqlite-web listening on %s (db=%s)", addr, dbPath)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
