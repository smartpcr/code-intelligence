package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
)

// decodeJWKEntry converts a single JWK entry (RSA or EC) into
// a `crypto`-package public key the JWT verifier can consume.
//
// RFC 7517 Sec 4 / 4.2 / 4.3 specifies the JWK shape; we
// implement decoding directly rather than dragging in
// `gopkg.in/square/go-jose` for the parsing alone -- the
// JWKS-rotation workflow only needs RSA + EC (the two
// algorithm families OIDC providers use).
func decodeJWKEntry(kty string, raw json.RawMessage) (any, error) {
	switch kty {
	case "RSA":
		return decodeRSAJWK(raw)
	case "EC":
		return decodeECJWK(raw)
	default:
		return nil, fmt.Errorf("unsupported JWK kty %q (only RSA, EC)", kty)
	}
}

type rsaJWK struct {
	N string `json:"n"`
	E string `json:"e"`
}

func decodeRSAJWK(raw json.RawMessage) (*rsa.PublicKey, error) {
	var k rsaJWK
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("decoding RSA JWK fields: %w", err)
	}
	if k.N == "" || k.E == "" {
		return nil, fmt.Errorf("RSA JWK missing `n` or `e`")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("base64-decoding RSA n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("base64-decoding RSA e: %w", err)
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, fmt.Errorf("RSA JWK has empty `n` or `e`")
	}
	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("RSA JWK `e` decoded to zero")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

type ecJWK struct {
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func decodeECJWK(raw json.RawMessage) (*ecdsa.PublicKey, error) {
	var k ecJWK
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("decoding EC JWK fields: %w", err)
	}
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", k.Crv)
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("base64-decoding EC x: %w", err)
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("base64-decoding EC y: %w", err)
	}
	if len(xBytes) == 0 || len(yBytes) == 0 {
		return nil, fmt.Errorf("EC JWK has empty `x` or `y`")
	}
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}
