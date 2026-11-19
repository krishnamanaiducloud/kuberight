package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// WriteCookieSecret writes a random session-cookie secret for an
// authenticating proxy sidecar to path, unless a non-empty one is already
// there. The secret is 32 hexadecimal characters, a length every supported
// proxy accepts as an AES key. The file is created owner-readable only and
// never overwritten, so a restarted init container keeps existing sessions.
func WriteCookieSecret(path string) error {
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generate the cookie secret: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.WriteString(hex.EncodeToString(secret)); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	return file.Close()
}
