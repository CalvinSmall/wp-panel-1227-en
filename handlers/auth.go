package handlers

import (
	"database/sql"
	"net/http"

	"github.com/naibabiji/wp-panel/i18n"
	"github.com/naibabiji/wp-panel/middleware"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func setSessionCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     "wp_session",
		Value:    token,
		MaxAge:   maxAge,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

type AuthHandler struct {
	DB      *sql.DB
	Prefix  string
	Tracker *middleware.LoginAttemptTracker
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(i18n.TE(c.Request, "auth.provide_credentials")))
		return
	}

	var hash string
	err := h.DB.QueryRow(
		"SELECT password_hash FROM admin_users WHERE username = ?", req.Username,
	).Scan(&hash)

	if err != nil {
		// Prevent timing attack: run a dummy verification
		bcrypt.CompareHashAndPassword([]byte("$2a$12$vI8aWBnW3fID.ZQ4/zo1G.q1lRps.9cGLcZEiGDMVr5yUP1KUOYTa"), []byte(req.Password))
		if h.Tracker != nil {
			h.Tracker.RecordAttempt(c.ClientIP(), "web_login")
		}
		c.JSON(http.StatusUnauthorized, models.ErrorResponse(i18n.TE(c.Request, "auth.invalid_credentials")))
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		if h.Tracker != nil {
			h.Tracker.RecordAttempt(c.ClientIP(), "web_login")
		}
		c.JSON(http.StatusUnauthorized, models.ErrorResponse(i18n.TE(c.Request, "auth.invalid_credentials")))
		return
	}

	if h.Tracker != nil {
		h.Tracker.ClearAttempts(c.ClientIP())
	}

	session := middleware.GlobalSessionStore.Create(req.Username)
	setSessionCookie(c, session.Token, 1800)

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"username": req.Username,
	}))
}

func (h *AuthHandler) Logout(c *gin.Context) {
	token, err := c.Cookie("wp_session")
	if err == nil && token != "" {
		middleware.GlobalSessionStore.Delete(token)
	}
	setSessionCookie(c, "", -1)
	c.JSON(http.StatusOK, models.SuccessResponse(nil))
}

func (h *AuthHandler) Check(c *gin.Context) {
	username, exists := c.Get("session_username")
	if !exists {
		c.JSON(http.StatusUnauthorized, models.ErrorResponse(i18n.TE(c.Request, "auth.not_logged_in")))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"username": username,
	}))
}

func (h *AuthHandler) CSRFToken(c *gin.Context) {
	token, err := c.Cookie("csrf_token")
	if err != nil || token == "" {
		c.JSON(http.StatusOK, models.ErrorResponse(i18n.TE(c.Request, "auth.missing_csrf")))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"token": token,
	}))
}
