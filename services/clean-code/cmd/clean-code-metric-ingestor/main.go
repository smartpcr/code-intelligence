// Package main is the entrypoint for the clean-code-metric-ingestor service.
// It processes commits, creates metric_sample rows, and manages the
// metric_sample_active pointer for active-row uniqueness enforcement.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		log.Fatal("CLEAN_CODE_PG_URL is required")
	}

	var err error
	db, err = sql.Open("postgres", pgURL)
	if err != nil {
		log.Fatalf("opening postgres: %v", err)
	}
	defer db.Close()

	// Wait for Postgres to be ready. If we exhaust the retry
	// budget without a successful Ping, fail fast: serving
	// HTTP traffic against an unreachable DB would just turn
	// every request into a 500 and hide the real failure.
	const pingAttempts = 30
	var pingErr error
	for i := 0; i < pingAttempts; i++ {
		if pingErr = db.Ping(); pingErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if pingErr != nil {
		log.Fatalf("postgres not reachable after %d attempts: %v", pingAttempts, pingErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/v1/ingestor/process", handleProcess)

	log.Printf("clean-code-metric-ingestor listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

type processRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
}

// handleProcess ingests metrics for a commit:
//  1. Creates a metric_sample row with a computed payload.
//  2. UPSERTs metric_sample_active to point at the new sample.
//
// Idempotency: if a metric_sample already exists for this SHA and the active
// pointer is not retracted, the handler skips re-computation (sample_id and
// row count remain unchanged). If retracted, a new sample is created and the
// pointer is moved.
func handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req processRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Check if an active, non-retracted sample already exists (computation-skip).
	var existingSampleID string
	err := db.QueryRow(`
		SELECT msa.sample_id
		FROM clean_code.metric_sample_active msa
		LEFT JOIN clean_code.metric_retraction mr ON mr.sample_id = msa.sample_id
		WHERE msa.commit_sha = $1 AND mr.sample_id IS NULL
	`, req.CommitSHA).Scan(&existingSampleID)

	if err == nil && existingSampleID != "" {
		// Active, non-retracted sample exists — skip re-computation.
		// sample_id and row count remain unchanged (idempotent).
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"skipped","sample_id":"%s"}`, existingSampleID)
		return
	}

	// Either no sample exists, or the current one is retracted — create new.
	var newSampleID string
	err = db.QueryRow(`
		INSERT INTO clean_code.metric_sample (commit_sha, payload)
		VALUES ($1, '{"source":"e2e-ingestor"}'::jsonb)
		RETURNING sample_id
	`, req.CommitSHA).Scan(&newSampleID)
	if err != nil {
		http.Error(w, "inserting metric_sample: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// UPSERT the active pointer to the new sample.
	_, err = db.Exec(`
		INSERT INTO clean_code.metric_sample_active (commit_sha, sample_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (commit_sha)
		DO UPDATE SET sample_id = EXCLUDED.sample_id, updated_at = now()
	`, req.CommitSHA, newSampleID)
	if err != nil {
		http.Error(w, "upserting metric_sample_active: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ingested","sample_id":"%s"}`, newSampleID)
}
