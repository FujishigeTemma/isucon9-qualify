package main

import (
	"net/http"
	"sync"
)

const cookieName = "session"

var store = NewSessionStore()

type SessionStore struct {
	mp map[string]SessionData
	mu sync.RWMutex
}

func NewSessionStore() SessionStore {
	return SessionStore{
		mp: make(map[string]SessionData),
		mu: sync.RWMutex{},
	}
}
func (s *SessionStore) Get(key string) (SessionData, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, ok := s.mp[key]
	return v, ok
}
func (s *SessionStore) Set(key string, value SessionData) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mp[key] = value
}

type SessionData struct {
	UserID    int64
	CsrfToken string
}

func getSession(r *http.Request) (SessionData, bool) {
	token, ok := readCookie(r)
	if !ok {
		return SessionData{}, false
	}

	return store.Get(token)
}

func setSession(w http.ResponseWriter, data SessionData) {
	token := secureRandomStr(32)
	writeCookie(w, token)

	store.Set(token, data)
}

func readCookie(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	return cookie.Value, true
}

func writeCookie(w http.ResponseWriter, token string) {
	cookie := &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		Domain:   "",
		Secure:   false,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}

	w.Header().Add("Set-Cookie", cookie.String())
	addHeaderIfMissing(w, "Cache-Control", `no-cache="Set-Cookie"`)
	addHeaderIfMissing(w, "Vary", "Cookie")
}

func addHeaderIfMissing(w http.ResponseWriter, key, value string) {
	for _, h := range w.Header()[key] {
		if h == value {
			return
		}
	}
	w.Header().Add(key, value)
}
