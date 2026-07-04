package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Session struct {
	Token     string
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

var GlobalSessionStore = &SessionStore{
	sessions: make(map[string]*Session),
}

func (s *SessionStore) Create(username string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	session := &Session{
		Token:     uuid.New().String(),
		Username:  username,
		CreatedAt: now,
		ExpiresAt: now.Add(30 * time.Minute),
	}
	s.sessions[session.Token] = session
	return session
}

func (s *SessionStore) Get(token string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[token]
	if !ok {
		return nil
	}
	if time.Now().After(session.ExpiresAt) {
		delete(s.sessions, token)
		return nil
	}
	// Sliding renewal: extend by 30 minutes on each valid access
	session.ExpiresAt = time.Now().Add(30 * time.Minute)
	return session
}

func (s *SessionStore) CleanExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for token, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, token)
		}
	}
}

func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func SessionRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie("wp_session")
		if err != nil || token == "" {
			abortSession(c, "Please log in first")
			return
		}

		session := GlobalSessionStore.Get(token)
		if session == nil {
			abortSession(c, "Session expired, please log in again")
			return
		}

		// Sliding renewal for client cookie
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     "wp_session",
			Value:    session.Token,
			MaxAge:   1800,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})

		c.Set("session_username", session.Username)
		c.Next()
	}
}

func abortSession(c *gin.Context, msg string) {
	c.SetCookie("wp_session", "", -1, "/", "", false, true)

	if isPageRequest(c) {
		prefix := extractPrefix(c)
		c.Redirect(http.StatusFound, prefix+"/login")
		return
	}

	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"success": false,
		"message": msg,
	})
}

func isPageRequest(c *gin.Context) bool {
	accept := c.GetHeader("Accept")
	return strings.Contains(accept, "text/html")
}

func extractPrefix(c *gin.Context) string {
	path := strings.Trim(c.Request.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) > 0 {
		return "/" + parts[0]
	}
	return ""
}
