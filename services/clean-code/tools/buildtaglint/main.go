// -----------------------------------------------------------------------
// <copyright file="main.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Command buildtaglint is the `go vet -vettool=...` entry
// point that ships the two custom analyzers required by
// `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
// Sec 8.10:
//
//   - `nobuildtagbypass`         -> rule no-production-build-tag-bypass
//   - `nocliproductionsqlimport` -> rule no-production-sql-import
//
// Invoked from `make lint-cli` as:
//
//go build -o bin/buildtaglint ./tools/buildtaglint
//go vet -vettool=$(pwd)/bin/buildtaglint \
//    ./cmd/cleanc/... ./internal/cli/...
//
// The two analyzers are composed under `multichecker.Main`
// so `go vet` invokes both in a single pass; a violation in
// either rule produces a `file:line:col: <rule-name>: <msg>`
// diagnostic on stderr and a non-zero exit status.
//
// See `tools/buildtaglint/README.md` for the full operator
// guide and extension recipe.
package main

import (
"golang.org/x/tools/go/analysis/multichecker"

bt "github.com/smartpcr/code-intelligence/services/clean-code/tools/buildtaglint/lint"
)

func main() {
multichecker.Main(
bt.BuildTagAnalyzer,
bt.SQLImportAnalyzer,
)
}
