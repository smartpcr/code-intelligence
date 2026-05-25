// Package main is the entrypoint for the clean-code-metric-ingestor service.
// It processes commits through scan recipes and manages the ScanRun lifecycle.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// validScanRunKinds enumerates the allowed scan_run kind values.
// The ingestor MUST reject any kind not in this set before reaching PostgreSQL.
var validScanRunKinds = map[string]bool{
	"ast_metrics": true,
	"lint":        true,
	"complexity":  true,
	"dependency":  true,
}

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

	// Wait for Postgres to be ready.
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/v1/ingestor/process", handleProcess)
	mux.HandleFunc("/v1/ingestor/scan-run", handleScanRun)

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
	Recipe    string `json:"recipe"`
}

// runRecipe executes the scan recipe for a commit. Panicking recipes are
// recovered by the caller — this models the real ingestor's behaviour where
// a bad recipe causes a Go panic that the service catches.
func runRecipe(commitSHA, recipe string) {
	if strings.Contains(recipe, "__panic_test__") {
		panic(fmt.Sprintf("recipe %q panicked on commit %s", recipe, commitSHA))
	}
}

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

	// Transition: pending -> scanning (committed to DB before any work begins
	// so that concurrent observers can witness the intermediate state).
	if _, err := db.Exec(`UPDATE clean_code.commit SET scan_status = 'scanning'::clean_code.scan_status, updated_at = now() WHERE sha = $1`, req.CommitSHA); err != nil {
		http.Error(w, "updating to scanning: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Brief yield so that concurrent DB pollers can observe the "scanning"
	// state before it transitions to scanned/failed.
	time.Sleep(100 * time.Millisecond)

	// Execute the recipe with panic recovery — models real ingestor behaviour
	// where a bad recipe panics and the service catches it via recover().
	var recipePanicked bool
	var panicValue interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				recipePanicked = true
				panicValue = r
			}
		}()
		runRecipe(req.CommitSHA, req.Recipe)
	}()

	if recipePanicked {
		log.Printf("recipe panicked: %v", panicValue)
		// Record a failed scan_run
		if _, err := db.Exec(`INSERT INTO clean_code.scan_run (commit_sha, kind, status, finished_at) VALUES ($1, 'ast_metrics'::clean_code.scan_run_kind, 'failed'::clean_code.scan_run_status, now())`, req.CommitSHA); err != nil {
			log.Printf("inserting failed scan_run: %v", err)
		}
		// Transition: scanning -> failed
		if _, err := db.Exec(`UPDATE clean_code.commit SET scan_status = 'failed'::clean_code.scan_status, updated_at = now() WHERE sha = $1`, req.CommitSHA); err != nil {
			log.Printf("updating to failed: %v", err)
		}
		http.Error(w, fmt.Sprintf("recipe panicked: %v", panicValue), http.StatusInternalServerError)
		return
	}

	// Happy path: record a succeeded scan_run
	if _, err := db.Exec(`INSERT INTO clean_code.scan_run (commit_sha, kind, status, finished_at) VALUES ($1, 'ast_metrics'::clean_code.scan_run_kind, 'succeeded'::clean_code.scan_run_status, now())`, req.CommitSHA); err != nil {
		http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Transition: scanning -> scanned
	if _, err := db.Exec(`UPDATE clean_code.commit SET scan_status = 'scanned'::clean_code.scan_status, updated_at = now() WHERE sha = $1`, req.CommitSHA); err != nil {
		http.Error(w, "updating to scanned: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

type scanRunRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
	Kind      string `json:"kind"`
}

func handleScanRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req scanRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Application-level enum guard: reject invalid kind BEFORE reaching PostgreSQL.
	if !validScanRunKinds[req.Kind] {
		http.Error(w, fmt.Sprintf("invalid scan_run kind %q: must be one of ast_metrics, lint, complexity, dependency", req.Kind), http.StatusBadRequest)
		return
	}

	if _, err := db.Exec(`INSERT INTO clean_code.scan_run (commit_sha, kind, status) VALUES ($1, $2::clean_code.scan_run_kind, 'running'::clean_code.scan_run_status)`,
		req.CommitSHA, req.Kind); err != nil {
		http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, `{"status":"created"}`)
}
