// Package main is the entrypoint for the clean-code-indexer service.
// It indexes repositories for code-quality analysis.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		log.Fatal("CLEAN_CODE_PG_URL is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	log.Printf("clean-code-indexer listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}