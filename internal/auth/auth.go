// Package auth validates the shared bearer key without logging secret values.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aisidev/tt/api"
)

func ParseAPIKey(key string) (string, error) {
	valid := len(key) > 0
	for _, c := range []byte(key) {
		if c < 0x21 || c > 0x7e {
			valid = false
		}
	}
	if !valid {
		return "", fmt.Errorf("API key must be nonempty printable ASCII without whitespace")
	}
	return key, nil
}
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func Middleware(key *string, next http.Handler) http.Handler {
	if key == nil {
		return next
	}
	expected := *key
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if ok && strings.EqualFold(scheme, "bearer") && ConstantTimeEqual(token, expected) {
			next.ServeHTTP(w, r)
			return
		}
		data, _ := json.Marshal(api.Failure[struct{}]("invalid or missing API key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(data)
	})
}
