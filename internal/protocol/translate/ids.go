package translate

import (
	"crypto/rand"
	"encoding/hex"
)

// randomID returns a short random token for synthesized ids (message/response ids when the
// upstream did not provide one, tool_use ids when a source protocol omitted them).
func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
