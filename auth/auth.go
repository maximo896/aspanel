package auth

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"strings"

	"awvs-sqlmap-panel/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	defaultAdminUsername = "admin"
	defaultAdminPassword = "admin123456"
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

func BasicAuthMiddleware(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		username, password, ok := c.Request.BasicAuth()
		if !ok {
			abortUnauthorized(c)
			return
		}
		var cred models.AdminCredential
		if err := db.Order("id asc").First(&cred).Error; err != nil {
			abortUnauthorized(c)
			return
		}
		if strings.TrimSpace(username) != strings.TrimSpace(cred.Username) {
			abortUnauthorized(c)
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(password)); err != nil {
			abortUnauthorized(c)
			return
		}
		c.Next()
	}
}

func abortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Basic realm="AWVS SQLMap Panel"`)
	c.AbortWithStatusJSON(401, gin.H{"error": "authentication required"})
}
