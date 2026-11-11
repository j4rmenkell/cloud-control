// Package interactions serves Discord slash commands over HTTP (Discord's
// "interactions endpoint"), so the bot needs no gateway connection and can
// run on AWS Lambda behind a public Function URL.
//
// Every request must carry a valid Ed25519 signature from Discord; anything
// else is rejected before it is parsed.
package interactions

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// MaxSkew bounds how old (or how far in the future) a signed request may be.
// Signatures alone don't stop replays of a captured request; a freshness
// window limits that.
const MaxSkew = 5 * time.Minute

var (
	ErrBadSignature = errors.New("invalid request signature")
	ErrStale        = errors.New("request timestamp outside allowed window")
)

// Verifier checks Discord's X-Signature-Ed25519 / X-Signature-Timestamp
// headers against the application's public key.
type Verifier struct {
	key ed25519.PublicKey
	now func() time.Time
}

// NewVerifier parses the hex public key from the Developer Portal.
func NewVerifier(publicKeyHex string, now func() time.Time) (*Verifier, error) {
	raw, err := hex.DecodeString(publicKeyHex)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("DISCORD_PUBLIC_KEY must be %d hex-encoded bytes", ed25519.PublicKeySize)
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{key: ed25519.PublicKey(raw), now: now}, nil
}

// Verify checks that body was signed by Discord at timestamp.
func (v *Verifier) Verify(signatureHex, timestamp string, body []byte) error {
	sig, err := hex.DecodeString(signatureHex)
	if err != nil || len(sig) != ed25519.SignatureSize || timestamp == "" {
		return ErrBadSignature
	}
	msg := make([]byte, 0, len(timestamp)+len(body))
	msg = append(msg, timestamp...)
	msg = append(msg, body...)
	if !ed25519.Verify(v.key, msg, sig) {
		return ErrBadSignature
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := v.now().Sub(time.Unix(secs, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrStale
	}
	return nil
}
