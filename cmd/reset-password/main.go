package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	dbPath := os.Args[1]
	username := os.Args[2]
	password := os.Args[3]

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		fmt.Println("Error opening db:", err)
		os.Exit(1)
	}
	defer db.Close()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Println("Error hashing password:", err)
		os.Exit(1)
	}

	_, err = db.Exec("UPDATE users SET password = ? WHERE username = ?", string(hash), username)
	if err != nil {
		fmt.Println("Error updating password:", err)
		os.Exit(1)
	}

	fmt.Println("Password reset successful for", username)
}
