// Package guestconfig handles opaque guest files without executing their contents.
package guestconfig

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/model"
)

const MaxConfigBytes = 64 * 1024
const ConfigDisk = "guest-config.ext4"

func DiskMiB(e model.Engine, callerFiles, sshSeed bool) uint32 {
	if e == model.Firecracker && (callerFiles || sshSeed) {
		return model.ConfigDiskMiB
	}
	return 0
}
func Validate(e model.Engine, files model.GuestConfig, isolated bool) error {
	if len(files) > 0 {
		if err := capability.GuestConfig.RequireDesign(e, model.File); err != nil {
			return err
		}
	}
	if isolated {
		if err := capability.Isolation.RequireDesign(e, model.File); err != nil {
			return err
		}
	}
	total := 0
	for k, v := range files {
		total += len(k) + len(v)
	}
	if len(files) > 32 || total > MaxConfigBytes {
		return fmt.Errorf("guest_config exceeds 32 files or 64 KiB")
	}
	for name := range files {
		valid := len(name) > 0 && len(name) <= 128 && !strings.HasPrefix(name, ".")
		for _, c := range []byte(name) {
			if !model.ASCIIAlphanumeric(c) && !strings.ContainsRune("-_.", rune(c)) {
				valid = false
			}
		}
		if !valid {
			return fmt.Errorf("guest_config keys must be simple file names (letters, digits, -, _, .; no leading dot)")
		}
	}
	return nil
}

// CanonicalJSON is serde_json's compact serialization of a BTreeMap<String,String>.
// In particular it does not HTML-escape or escape Unicode line separators.
func CanonicalJSON(files model.GuestConfig) []byte {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := []byte{'{'}
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendString(out, k)
		out = append(out, ':')
		out = appendString(out, files[k])
	}
	return append(out, '}')
}
func appendString(out []byte, s string) []byte {
	out = append(out, '"')
	const hex = "0123456789abcdef"
	for _, c := range []byte(s) {
		switch c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 32 {
				out = append(out, '\\', 'u', '0', '0', hex[c>>4], hex[c&15])
			} else {
				out = append(out, c)
			}
		}
	}
	return append(out, '"')
}
func Digest(files model.GuestConfig) *string {
	if len(files) == 0 {
		return nil
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(CanonicalJSON(files)))
	return &digest
}
