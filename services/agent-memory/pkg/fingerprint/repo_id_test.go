package fingerprint

import (
	"errors"
	"testing"
)

// pinnedVectors are the golden RFC 4122 v5 UUIDs that
// RepoIDFromURL MUST derive for the canonical test inputs. The
// strings here are not aesthetic choices: they are byte-for-byte
// outputs of `uuid.NewSHA1(namespaceRepoURL, []byte(url))` with the
// pinned namespace constant in repo_id.go. Any change to the
// namespace (or to the upstream uuid v5 implementation) re-keys every
// previously-scanned repo and BREAKS this test on purpose — see
// architecture REPO-SCANNER S3.4 for why the pinning matters
// (backend-parity across Postgres / SQLite / memory sinks).
//
// To regenerate (deliberate namespace rotation only), temporarily
// add a discovery test calling t.Logf with id.String() for each URL,
// run `go test -run TestDiscover -v ./pkg/fingerprint/`, paste the
// output here, delete the discovery test.
var pinnedVectors = []struct {
	name string
	url  string
	want string
}{
	{
		name: "https-github",
		url:  "https://github.com/foo/bar",
		want: "2c42b442-c304-5b86-99b7-e98998f2752d",
	},
	{
		name: "ssh-git",
		url:  "git@github.com:foo/bar.git",
		want: "a4d7a68c-b918-5dec-a6b4-87a4fdf9e517",
	},
	{
		name: "file-url",
		url:  "file:///c/code/repo",
		want: "80d5453f-1e29-5465-bc94-32dd86d9530c",
	},
}

// TestRepoIDFromURL_pinnedVectors locks the deterministic v5 UUID
// outputs of RepoIDFromURL for at least three representative repo
// URL forms (https, ssh-style git, file://). Drift in the namespace
// constant, the UUID library, or the input bytes flips this test
// red — that's the point (impl-plan Stage 2.1 step 2 / e2e scenario
// `repoid-deterministic-for-same-url`).
func TestRepoIDFromURL_pinnedVectors(t *testing.T) {
	t.Parallel()
	for _, v := range pinnedVectors {
		v := v
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			got, err := RepoIDFromURL(v.url)
			if err != nil {
				t.Fatalf("RepoIDFromURL(%q): %v", v.url, err)
			}
			if got.String() != v.want {
				t.Errorf(
					"RepoIDFromURL(%q) drifted:\n got = %s\nwant = %s\n"+
						"(if this is a DELIBERATE namespace rotation, see "+
						"the regen note on pinnedVectors)",
					v.url, got.String(), v.want,
				)
			}
			// A v5 UUID has the high nibble of byte 6 == 0x5 and the
			// top two bits of byte 8 == 0b10. Verify both — a derivation
			// that drifted to v4/v3 would still pass the string compare
			// only if the namespace also changed, but the assert costs
			// nothing and documents the contract.
			if got[6]&0xf0 != 0x50 {
				t.Errorf("byte[6]=0x%02x: version nibble not 5 (got %x)",
					got[6], got[6]>>4)
			}
			if got[8]&0xc0 != 0x80 {
				t.Errorf("byte[8]=0x%02x: variant bits not RFC 4122 (10)",
					got[8])
			}
		})
	}
}

// TestRepoIDFromURL_determinism_sameProcess is the impl-plan
// Stage 2.1 scenario "deterministic-for-same-url" reduced to a
// single-process round-trip: two calls with the same URL produce
// byte-identical RepoIDs. The cross-process half is covered
// implicitly by TestRepoIDFromURL_pinnedVectors (the pinned hex
// strings are what other processes / language runtimes will compute
// from the same algorithm).
func TestRepoIDFromURL_determinism_sameProcess(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/foo/bar"
	a, err := RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	b, err := RepoIDFromURL(url)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a != b {
		t.Errorf("RepoIDFromURL not deterministic:\n a=%s\n b=%s",
			a.String(), b.String())
	}
}

// TestRepoIDFromURL_differentURLsDiverge is the impl-plan Stage 2.1
// scenario "different-urls-diverge": two URLs differing by one
// character must produce different RepoIDs. The test catches a
// hypothetical bug where the helper hashed only a prefix of the URL
// or normalised it down to a common form by accident.
func TestRepoIDFromURL_differentURLsDiverge(t *testing.T) {
	t.Parallel()
	a, err := RepoIDFromURL("https://github.com/foo/bar")
	if err != nil {
		t.Fatalf("bar: %v", err)
	}
	b, err := RepoIDFromURL("https://github.com/foo/baz")
	if err != nil {
		t.Fatalf("baz: %v", err)
	}
	if a == b {
		t.Errorf("URLs differing by one character collided:\n a=%s\n b=%s",
			a.String(), b.String())
	}
}

// TestRepoIDFromURL_emptyURLRejected is the impl-plan Stage 2.1
// scenario "empty-url-rejected": calling with the empty string must
// return ErrEmptyURL AND a zero RepoID. The zero RepoID half matters
// because graphwriter.EnsureRepoWithID and the SQLite/memory sinks
// use RepoID.IsZero() as a precondition guard; if RepoIDFromURL ever
// "succeeded" with the zero value on bad input, those guards would
// silently fail open.
func TestRepoIDFromURL_emptyURLRejected(t *testing.T) {
	t.Parallel()
	got, err := RepoIDFromURL("")
	if err == nil {
		t.Fatalf("RepoIDFromURL(\"\") returned no error; want ErrEmptyURL")
	}
	if !errors.Is(err, ErrEmptyURL) {
		t.Errorf("err = %v; want errors.Is(err, ErrEmptyURL) == true", err)
	}
	if !got.IsZero() {
		t.Errorf("RepoIDFromURL(\"\") returned non-zero RepoID %s on error",
			got.String())
	}
}

// TestRepoIDFromURL_namespaceConstantPinned guards against an
// accidental edit of namespaceRepoURL. The literal string here MUST
// match the constant in repo_id.go byte-for-byte; if you intend to
// rotate the namespace, update BOTH and regenerate pinnedVectors.
// This test exists so the rotation cannot land as a silent
// one-character typo in either file.
func TestRepoIDFromURL_namespaceConstantPinned(t *testing.T) {
	t.Parallel()
	const want = "7e9a3d4c-1f5b-4d8e-9a2b-3c4d5e6f7a8b"
	if got := namespaceRepoURL.String(); got != want {
		t.Errorf("namespaceRepoURL drifted: got %s, want %s\n"+
			"(this constant pins every previously-derived RepoID; "+
			"changing it re-keys the universe — see repo_id.go)",
			got, want)
	}
}
