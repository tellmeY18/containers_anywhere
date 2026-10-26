package guest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/tellmeY18/containers_anywhere/internal/atomicfile"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(path, b)
}
