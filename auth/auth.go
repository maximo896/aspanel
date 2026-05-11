package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"encoding/hex"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"awvs-sqlmap-panel/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	defaultAdminUsername = "admin"
	defaultAdminPassword = "admin123456"
	sessionCookieName    = "panel_session" // legacy fallback
	sessionTTL           = 7 * 24 * time.Hour
	sessionRefreshWindow = 24 * time.Hour
)

func EnsureDefaultAdminCredential(db *gorm.DB) error {
	var count int64
	if err := db.Model(&models.AdminCredential{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(defaultAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	cred := models.AdminCredential{
		Username:     defaultAdminUsername,
		PasswordHash: string(hash),
	}
	if err := db.Create(&cred).Error; err != nil {
		return err
	}
	log.Printf("[auth] initialized default admin credential username=%s password=%s", defaultAdminUsername, defaultAdminPassword)
	return nil
}

func ResetAdminCredential(db *gorm.DB, username, password string) error {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" {
		return errors.New("username is required")
	}
	if password == "" {
		return errors.New("password is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	var cred models.AdminCredential
	err = db.Order("id asc").First(&cred).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		cred = models.AdminCredential{
			Username:     username,
			PasswordHash: string(hash),
		}
		return db.Create(&cred).Error
	}
	if err != nil {
		return err
	}
	cred.Username = username
	cred.PasswordHash = string(hash)
	return db.Save(&cred).Error
}

func HandleCLI(db *gorm.DB, args []string) (bool, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) != "reset-admin" {
		return false, nil
	}
	cmd := flag.NewFlagSet("reset-admin", flag.ContinueOnError)
	cmd.SetOutput(io.Discard)
	username := cmd.String("username", "", "admin username")
	password := cmd.String("password", "", "admin password")
	if err := cmd.Parse(args[1:]); err != nil {
		return true, fmt.Errorf("invalid reset-admin args: %w", err)
	}
	if err := ResetAdminCredential(db, *username, *password); err != nil {
		return true, err
	}
	log.Printf("[auth] admin credential reset success username=%s", strings.TrimSpace(*username))
	return true, nil
}

func SessionAuthMiddleware(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/" || strings.HasPrefix(path, "/static/") || path == "/api/auth/login" {
			c.Next()
			return
		}

		rawToken, err := getSessionTokenFromRequest(c)
		if err != nil || strings.TrimSpace(rawToken) == "" {
			abortUnauthorized(c)
			return
		}

		tokenHash := hashToken(rawToken)
		var sess models.AdminSession
		if err := db.Where("token_hash = ?", tokenHash).First(&sess).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				abortUnauthorized(c)
				return
			}
			abortAuthStorageError(c)
			return
		}
		if sess.ExpiresAt > 0 && time.Now().Unix() >= sess.ExpiresAt {
			db.Delete(&sess)
			clearSessionCookie(c)
			abortUnauthorized(c)
			return
		}
		extendSession(db, &sess)
		c.Next()
	}
}

func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(401, gin.H{"error": "authentication required"})
}

func abortAuthStorageError(c *gin.Context) {
	c.AbortWithStatusJSON(503, gin.H{"error": "auth storage unavailable"})
}

func RegisterRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/api/auth/login", func(c *gin.Context) {
		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var cred models.AdminCredential
		if err := db.Order("id asc").First(&cred).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		if strings.TrimSpace(req.Username) != strings.TrimSpace(cred.Username) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.Password)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}

		token, err := generateToken()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
			return
		}
		expiresAt := time.Now().Add(sessionTTL).Unix()
		sess := models.AdminSession{
			TokenHash: hashToken(token),
			ExpiresAt: expiresAt,
		}
		if err := db.Create(&sess).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
			return
		}
		setSessionCookie(c, token)
		c.JSON(http.StatusOK, gin.H{
			"message":  "login success",
			"username": cred.Username,
		})
	})

	r.POST("/api/auth/logout", func(c *gin.Context) {
		rawToken, _ := getSessionTokenFromRequest(c)
		if strings.TrimSpace(rawToken) != "" {
			db.Where("token_hash = ?", hashToken(rawToken)).Delete(&models.AdminSession{})
		}
		clearSessionCookie(c)
		c.JSON(http.StatusOK, gin.H{"message": "logout success"})
	})

	r.GET("/api/auth/me", func(c *gin.Context) {
		rawToken, err := getSessionTokenFromRequest(c)
		if err != nil || strings.TrimSpace(rawToken) == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		tokenHash := hashToken(rawToken)
		var sess models.AdminSession
		if err := db.Where("token_hash = ?", tokenHash).First(&sess).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				clearSessionCookie(c)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth storage unavailable"})
			return
		}
		if sess.ExpiresAt > 0 && time.Now().Unix() >= sess.ExpiresAt {
			db.Delete(&sess)
			clearSessionCookie(c)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		extendSession(db, &sess)

		var cred models.AdminCredential
		_ = db.Order("id asc").First(&cred).Error
		c.JSON(http.StatusOK, gin.H{
			"username": strings.TrimSpace(cred.Username),
		})
	})
}

func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func setSessionCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieNameByRequest(c), token, int(sessionTTL.Seconds()), "/", "", false, true)
}

func clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(sessionCookieNameByRequest(c), "", -1, "/", "", false, true)
	// Clear legacy cookie key as well.
	c.SetCookie(sessionCookieName, "", -1, "/", "", false, true)
}

func extendSession(db *gorm.DB, sess *models.AdminSession) {
	if sess == nil {
		return
	}
	now := time.Now()
	remaining := time.Unix(sess.ExpiresAt, 0).Sub(now)
	if sess.ExpiresAt > 0 && remaining > sessionRefreshWindow {
		return
	}
	newExpires := now.Add(sessionTTL).Unix()
	if newExpires == sess.ExpiresAt {
		return
	}
	sess.ExpiresAt = newExpires
	_ = db.Model(sess).Update("expires_at", newExpires).Error
}

func sessionCookieNameByRequest(c *gin.Context) string {
	host := strings.TrimSpace(c.Request.Host)
	if host == "" {
		return sessionCookieName
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(strings.ToLower(host)))
	return fmt.Sprintf("%s_%08x", sessionCookieName, hasher.Sum32())
}

func getSessionTokenFromRequest(c *gin.Context) (string, error) {
	// 1) Prefer host-scoped cookie name to avoid cross-instance overwrite.
	if token, err := c.Cookie(sessionCookieNameByRequest(c)); err == nil && strings.TrimSpace(token) != "" {
		return token, nil
	}
	// 2) Backward compatibility for legacy fixed-name cookie.
	return c.Cookie(sessionCookieName)
}
