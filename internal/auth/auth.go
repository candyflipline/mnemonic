// Package auth maps bearer tokens to authors. The server stores only the
// sha256 of each token; the author a request writes as comes from its token,
// never from the request.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

type ctxKey struct{}

// Hash is the form tokens take in the server config.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Middleware rejects requests without a known token and puts the token's
// author on the context. authors maps Hash(token) to author.
func Middleware(authors map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			author, known := authors[Hash(token)]
			if !ok || !known {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, author)))
		})
	}
}

// Author is the authenticated author; handlers behind Middleware always have one.
func Author(ctx context.Context) string {
	a, _ := ctx.Value(ctxKey{}).(string)
	return a
}
