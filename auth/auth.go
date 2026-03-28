package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

// OpenDB opens (or creates) the SQLite database at dbPath and ensures the schema exists.
func OpenDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Serialise all access through one connection — SQLite does not support
	// concurrent writers and will return "database is locked" otherwise.
	db.SetMaxOpenConns(1)

	if err := initDB(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initDB(db *sql.DB) error {
	// Enable foreign key enforcement (disabled by default in SQLite).
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			username      TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL,
			created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("creating users table: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tokens (
			token      TEXT PRIMARY KEY,
			username   TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (username) REFERENCES users(username) ON DELETE CASCADE
		)
	`); err != nil {
		return fmt.Errorf("creating tokens table: %w", err)
	}
	return nil
}

// AddUser creates a new user with a bcrypt-hashed password.
func AddUser(db *sql.DB, username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	_, err = db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, username, string(hash))
	if err != nil {
		return fmt.Errorf("adding user: %w", err)
	}
	return nil
}

// UpdateUser replaces an existing user's password.
func UpdateUser(db *sql.DB, username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	res, err := db.Exec(`UPDATE users SET password_hash = ? WHERE username = ?`, string(hash), username)
	if err != nil {
		return fmt.Errorf("updating user: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return errors.New("user not found")
	}
	return nil
}

// ListUsers returns all usernames sorted alphabetically.
func ListUsers(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT username FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// ValidateCredentials checks a plaintext password against the stored hash.
func ValidateCredentials(db *sql.DB, username, password string) (bool, error) {
	var hash string
	err := db.QueryRow(`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return err == nil, err
}

// CreateToken generates a random Bearer token for the given user and persists it.
func CreateToken(db *sql.DB, username string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	token := hex.EncodeToString(b)
	_, err := db.Exec(`INSERT INTO tokens (token, username) VALUES (?, ?)`, token, username)
	if err != nil {
		return "", fmt.Errorf("storing token: %w", err)
	}
	return token, nil
}

// tokenTTL is how long a Bearer token remains valid after creation.
const tokenTTL = "-90 days"

// ValidateToken returns the username associated with the Bearer token, or "" if
// the token is invalid or older than 90 days.
func ValidateToken(db *sql.DB, token string) (string, error) {
	var username string
	err := db.QueryRow(
		`SELECT username FROM tokens WHERE token = ? AND created_at > datetime('now', ?)`,
		token, tokenTTL,
	).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return username, err
}
