package main

import (
	"crypto/sha256"
	"encoding/hex"
)

// placeholderLFDI returns a deterministic 40-character lowercase hex
// string derived from the supplied mRID. It is a Stage 1 stand-in for
// the real IEEE 2030.5 long-form device ID, which is the truncated
// SHA-256 of a device certificate's DER bytes.
//
// Stage 2 (GAGO-026) replaces this with the real LFDI mapping derived
// from each device's enrolled certificate. Until then the bridge
// populates the registry with a synthetic LFDI so the rest of the
// pipeline (mRID-to-LFDI lookups, lookup-by-LFDI on inbound 2030.5
// requests) has a stable index to exercise.
//
// The placeholder length matches the real LFDI's 40-hex-character
// (160-bit) shape so that downstream code wired against the real LFDI
// width does not need to special-case the placeholder.
func placeholderLFDI(mrid string) string {
	sum := sha256.Sum256([]byte(mrid))
	return hex.EncodeToString(sum[:20])
}
