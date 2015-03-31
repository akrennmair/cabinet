package basicauth

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
)

type Handler struct {
	h         http.Handler
	authFunc  AuthenticatorFunc
	endpoints map[string]struct{}
}

type AuthenticatorFunc func(username, password string) bool

func NewHandler(h http.Handler, authFunc AuthenticatorFunc, endpoints []string) *Handler {
	authHandler := &Handler{
		h:        h,
		authFunc: authFunc,
	}
	for _, e := range endpoints {
		authHandler.endpoints[e] = struct{}{}
	}
	return authHandler
}

const basicAuthPrefix string = "Basic "

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, found := h.endpoints[r.URL.Path]; found {
		if Authenticate(w, r, h.authFunc) {
			h.h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Basic realm=Restricted")
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	h.h.ServeHTTP(w, r)
}

func Authenticate(w http.ResponseWriter, r *http.Request, authFunc AuthenticatorFunc) bool {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, basicAuthPrefix) {
		payload, err := base64.StdEncoding.DecodeString(auth[len(basicAuthPrefix):])
		if err == nil {
			pair := bytes.SplitN(payload, []byte(":"), 2)
			if len(pair) == 2 && authFunc(string(pair[0]), string(pair[1])) {
				return true
			}
		}
	}

	if w != nil {
		w.Header().Set("WWW-Authenticate", "Basic realm=Restricted")
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
	}
	return false
}
