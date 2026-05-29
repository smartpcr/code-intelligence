package composition

import (
	"database/sql"
	"fmt"
	"log/slog"

	"forge/services/clean-code/internal/management"
	"forge/services/clean-code/internal/metric_ingestor"
)

// BuildMgmtWriter assembles the production
// [management.MgmtWriter] for the four canonical write
// verbs (`mgmt.register_repo`, `mgmt.set_mode`,
// `mgmt.retract_sample`, `mgmt.rescan`).
//
// The helper takes TWO `*sql.DB` handles to honour the
// documented role boundary from
// `migrations/0004_roles.up.sql`:
//
//   - ingestorDB carries clean_code_metric_ingestor
//     credentials. Used for scan_run INSERT/UPDATE,
//     metric_retraction INSERT/SELECT, and metric_sample
//     SELECT (per migrations/0004 lines 282, 348, 374).
//   - mgmtDB carries clean_code_management credentials.
//     Used for repo INSERT/UPDATE (per-column grants in
//     0004:311-312 + 0006:140-141) AND repo_event INSERT
//     (0004:313).
//
// A composition root that wants to collapse the boundary
// (e.g. dev / E2E with the operator's opt-in
// `CLEAN_CODE_ALLOW_SHARED_PG_ROLE`) passes the SAME
// `*sql.DB` for both arguments. The role boundary is
// enforced by the credentials on the handle, NOT by this
// helper.
//
// Returns a non-nil error if either input is nil or any
// underlying store constructor fails. The returned writer
// has the Stage 6.2 `RepoStore` already plumbed so the new
// `register_repo` / `set_mode` verbs can persist.
func BuildMgmtWriter(ingestorDB, mgmtDB *sql.DB, logger *slog.Logger) (*management.MgmtWriter, error) {
	if ingestorDB == nil {
		return nil, fmt.Errorf("BuildMgmtWriter: ingestorDB is nil")
	}
	if mgmtDB == nil {
		return nil, fmt.Errorf("BuildMgmtWriter: mgmtDB is nil (mgmt-role handle is required)")
	}
	retractStore, err := metric_ingestor.NewPGRetractionStore(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGRetractionStore: %w", err)
	}
	retractScanRunStore, err := metric_ingestor.NewPGRetractScanRunStore(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGRetractScanRunStore: %w", err)
	}
	rescanStore, err := metric_ingestor.NewPGRescanScanRunStore(ingestorDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGRescanScanRunStore: %w", err)
	}
	appender, err := management.NewPGRepoEventAppender(mgmtDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGRepoEventAppender: %w", err)
	}
	// Stage 6.2: the PG-backed RepoStore writes
	// `clean_code.repo` AND the matching repo_event audit
	// row in ONE transaction. Same mgmtDB handle as the
	// appender so both writes run under the
	// clean_code_management role's grants.
	repoStore, err := management.NewPGRepoStore(mgmtDB)
	if err != nil {
		return nil, fmt.Errorf("NewPGRepoStore: %w", err)
	}
	dispatcher := metric_ingestor.NewRetractDispatcher(retractScanRunStore, retractStore, retractStore)
	enqueuer := metric_ingestor.NewRescanEnqueuer(rescanStore)
	if logger == nil {
		logger = slog.Default()
	}
	writer := management.NewMgmtWriter(
		// PGRetractionStore satisfies the management
		// SampleResolver interface directly -- no
		// adapter needed (structural typing).
		retractStore,
		management.AdaptMetricIngestorRetractDispatcher(dispatcher),
		management.AdaptMetricIngestorRescanEnqueuer(enqueuer),
		appender,
		management.WithMgmtWriterLogger(logger),
		management.WithMgmtWriterRepoStore(repoStore),
	)
	return writer, nil
}
