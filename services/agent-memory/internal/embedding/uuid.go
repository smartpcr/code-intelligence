package embedding

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewUUIDv4 returns a fresh RFC 4122 v4 UUID rendered in the
// canonical 8-4-4-4-12 lowercase hex form.  It exists here, in
// the embedding package, so the publisher can mint a Qdrant
// `point_id` synchronously (the production §9.6a flow needs
// the point id BEFORE the embedding_publish row is inserted so
// the PostgreSQL row and the eventual Qdrant point share the
// same identifier).
//
// Why not the standard library?  Go 1.24 still has no
// `uuid.NewRandom` in std.  Pulling in `github.com/google/uuid`
// for one v4 mint would add a runtime dependency for a 12-line
// helper.  The implementation below uses `crypto/rand` (which
// every existing call site in this service already trusts) and
// follows RFC 4122 §4.4 verbatim: version bits 0100 in byte 6,
// variant bits 10 in byte 8.
//
// Tests that need a deterministic id (e.g. assert
// "publisher minted point_id X") override `Publisher.newUUID`
// rather than seeding crypto/rand globally.
func NewUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("embedding: rand.Read for uuid: %w", err)
	}
	// Version 4 (random) -- high nibble of byte 6 must be 0100.
	b[6] = (b[6] & 0x0f) | 0x40
	// Variant RFC 4122 -- top two bits of byte 8 must be 10.
	b[8] = (b[8] & 0x3f) | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}
