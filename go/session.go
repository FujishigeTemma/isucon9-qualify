package main

import (
	"net/http"
	"sync"
)

const cookieName = "session"
const SessionStoreShards = 256

var store = NewSessionStore()

type SessionStoreShard struct {
	mp map[string]SessionData
	sync.RWMutex
}
type SessionStore []*SessionStoreShard

func NewSessionStore() SessionStore {
	m := make(SessionStore, SessionStoreShards)
	for i := 0; i < SessionStoreShards; i++ {
		shard := NewSessionStoreShard()
		m[i] = &shard
	}
	return m
}
func NewSessionStoreShard() SessionStoreShard {
	mp := make(map[string]SessionData, 50)
	return SessionStoreShard{mp: mp}
}

func (s SessionStore) GetShard(key string) *SessionStoreShard {
	return s[uint(fnv32(key))%uint(SessionStoreShards)]
}
func (s SessionStore) Get(key string) (SessionData, bool) {
	sh := s.GetShard(key)
	sh.RLock()
	d, ok := sh.mp[key]
	sh.RUnlock()
	return d, ok
}
func (s SessionStore) Set(key string, d SessionData) {
	sh := s.GetShard(key)
	sh.Lock()
	sh.mp[key] = d
	sh.Unlock()
}

func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < len(key); i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
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
