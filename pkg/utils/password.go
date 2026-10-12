package utils

import (
	"log"

	"golang.org/x/crypto/bcrypt"
)

// Bcrypt limits passwords by UTF-8 bytes, not rune count.
func ValidPasswordLength(password string) bool {
	return len(password) > 0 && len(password) <= 72
}

func HashPassword(password string) string {
	if !ValidPasswordLength(password) {
		return ""
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("HashPassword error: %v", err)
		return ""
	}
	return string(hash)
}

func CheckPassword(password, hash string) bool {
	if !ValidPasswordLength(password) {
		return false
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
