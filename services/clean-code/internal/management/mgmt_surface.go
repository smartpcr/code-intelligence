package management

// Stage 6.2 -- unified Management surface composition function.
//
// The brief's third bullet -- "Re-export `mgmt.retract_sample`
// (Stage 3.4), `mgmt.rescan` (Stage 3.4), `mgmt.override`
// (Stage 5.3) under the Management Surface namespace" --
// asks for a single canonical mount point that exposes EVERY
// `mgmt.*` write verb. Without this, an operator wiring the
// service has to consult two separate `Routes()` methods
// (`MgmtWriter.Routes()` for retract/rescan/register/set_mode
// and `PolicyWriter.Routes()` for override) and remember to
// mount both -- which is exactly the surface-bifurcation the
// brief calls out.
//
// [MgmtSurfaceRoutes] is that single mount point. Pass both
// writers (each MAY be nil for scaffold-mode bring-ups); the
// function mounts every `mgmt.*` verb whose backing writer is
// non-nil and returns an `http.ServeMux` ready to attach
// under any prefix the composition root chooses.
//
// # Why a function, not a constructor
//
// The mux is owned by the composition root, not the
// management package. Returning a fresh `*http.ServeMux`
// (rather than carrying a long-lived `*MgmtSurface` struct
// with its own constructor) keeps the management package's
// public surface narrow and matches the existing
// `MgmtWriter.Routes()` / `PolicyWriter.Routes()` /
// `Handler.Routes()` pattern.
//
// # Why not couple MgmtWriter <-> PolicyWriter
//
// An earlier design considered making `MgmtWriter.Routes()`
// mount override too by giving `MgmtWriter` a
// `*PolicyWriter` field. Rejected: `PolicyWriter`'s
// dependency tree (steward + signing keys) is unrelated to
// the dispatcher / enqueuer / repo-store deps the
// `MgmtWriter` carries, and conflating them couples two
// independent composition-root choices. The function-based
// composition keeps each writer self-contained.

import "net/http"

// MgmtSurfaceRoutes mounts the unified Management surface at
// the canonical `mgmt.*` paths and returns the configured
// mux. The composition root attaches the mux directly to its
// HTTP listener (or under a prefix):
//
//	mux := management.MgmtSurfaceRoutes(mgmtWriter, policyWriter)
//	http.Handle("/", mux)
//
// Mounted verbs (each conditional on its backing writer
// being non-nil):
//
//   - `mgmt.register_repo`  -- mgmtWriter, Stage 6.2
//   - `mgmt.set_mode`       -- mgmtWriter, Stage 6.2
//   - `mgmt.retract_sample` -- mgmtWriter, Stage 3.4
//   - `mgmt.rescan`         -- mgmtWriter, Stage 3.4
//   - `mgmt.override`       -- policyWriter, Stage 5.3
//
// `mgmt.register_repo` and `mgmt.set_mode` ALSO require a
// non-nil [RepoStore] on the writer (via
// [WithMgmtWriterRepoStore]); without it, hitting those
// paths returns 503 via the handler's own guard.
//
// Either writer MAY be nil; the affected routes are simply
// not mounted (a stray caller hitting the path lands on the
// default 404, not 503 -- the verb "doesn't exist here at
// all" rather than "the backing subsystem is down"). When
// BOTH are nil the function returns an empty mux.
//
// The mux includes ONLY mgmt.* verbs. The reader-side
// `policy.keys.list_active` (Stage 5.1) lives on the
// separate `Handler.Routes()`; the policy.* write verbs
// (`policy.publish`, `policy.activate`,
// `policy.publish_rulepack`) live on
// `PolicyWriter.Routes()`. Composition roots that want
// every read+write surface MOUNT all three muxes onto a
// parent mux.
func MgmtSurfaceRoutes(mgmtWriter *MgmtWriter, policyWriter *PolicyWriter) *http.ServeMux {
	mux := http.NewServeMux()
	if mgmtWriter != nil {
		mux.HandleFunc(VerbMgmtRetractSamplePath, mgmtWriter.RetractSample)
		mux.HandleFunc(VerbMgmtRescanPath, mgmtWriter.Rescan)
		// Stage 6.2: register_repo + set_mode mount
		// only when the RepoStore is wired (otherwise
		// the handlers return 503 anyway, so the extra
		// mount adds no value). The check is on the
		// writer's repoStore field; we keep the wire
		// surface honest by NOT mounting paths the
		// handler cannot serve.
		if mgmtWriter.repoStore != nil {
			mux.HandleFunc(VerbMgmtRegisterRepoPath, mgmtWriter.RegisterRepo)
			mux.HandleFunc(VerbMgmtSetModePath, mgmtWriter.SetMode)
		}
	}
	if policyWriter != nil {
		mux.HandleFunc(VerbMgmtOverridePath, policyWriter.Override)
	}
	return mux
}

// MgmtSurfaceVerbPaths returns the closed set of canonical
// `mgmt.*` HTTP paths in deterministic order. Pinned here
// so a conformance test can pull the list with a single
// import rather than re-typing each constant. Order is
// pinned for log / dashboard determinism.
//
// The set is the architecture's complete `mgmt.*` write
// surface per impl-plan line 21:
//
//	{mgmt.register_repo, mgmt.set_mode, mgmt.retract_sample,
//	 mgmt.rescan, mgmt.override}
//
// `mgmt.read.*` is NOT in this list -- those are read
// verbs and live on the reader-side `Handler` (Stage 6.3
// follow-up).
func MgmtSurfaceVerbPaths() []string {
	return []string{
		VerbMgmtRegisterRepoPath,
		VerbMgmtSetModePath,
		VerbMgmtRetractSamplePath,
		VerbMgmtRescanPath,
		VerbMgmtOverridePath,
	}
}
