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

// TokenInfo holds the details of a verified Bearer token.
type TokenInfo struct {
	Username string
	ClientID string
	Scope    string
}

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
			client_id  TEXT NOT NULL DEFAULT '',
			scope      TEXT NOT NULL DEFAULT 'create update delete',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (username) REFERENCES users(username) ON DELETE CASCADE
		)
	`); err != nil {
		return fmt.Errorf("creating tokens table: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS auth_codes (
			code         TEXT PRIMARY KEY,
			username     TEXT NOT NULL,
			client_id    TEXT NOT NULL,
			redirect_uri TEXT NOT NULL,
			scope        TEXT NOT NULL,
			created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (username) REFERENCES users(username) ON DELETE CASCADE
		)
	`); err != nil {
		return fmt.Errorf("creating auth_codes table: %w", err)
	}

	// Best-effort migrations: add columns introduced in this version to
	// pre-existing databases. SQLite returns an error if the column already
	// exists; we ignore it.
	db.Exec(`ALTER TABLE tokens ADD COLUMN client_id TEXT NOT NULL DEFAULT ''`)  //nolint:errcheck
	db.Exec(`ALTER TABLE tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'create update delete'`) //nolint:errcheck

	return nil
}

// ── User management ───────────────────────────────────────────────────────────

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

// ── Authorization code flow ───────────────────────────────────────────────────

// CreateAuthCode issues a short-lived authorization code (10-minute TTL).
func CreateAuthCode(db *sql.DB, username, clientID, redirectURI, scope string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating auth code: %w", err)
	}
	code := hex.EncodeToString(b)
	_, err := db.Exec(
		`INSERT INTO auth_codes (code, username, client_id, redirect_uri, scope) VALUES (?, ?, ?, ?, ?)`,
		code, username, clientID, redirectURI, scope,
	)
	if err != nil {
		return "", fmt.Errorf("storing auth code: %w", err)
	}
	return code, nil
}

// ExchangeAuthCode validates and consumes an authorization code, returning the
// username and scope. The code must match client_id and redirect_uri and must
// not be older than 10 minutes. Codes are single-use.
func ExchangeAuthCode(db *sql.DB, code, clientID, redirectURI string) (username, scope string, err error) {
	err = db.QueryRow(`
		SELECT username, scope FROM auth_codes
		WHERE code = ?
		  AND client_id = ?
		  AND redirect_uri = ?
		  AND created_at > datetime('now', '-10 minutes')
	`, code, clientID, redirectURI).Scan(&username, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("invalid or expired authorization code")
	}
	if err != nil {
		return "", "", err
	}
	// Codes are single-use; delete immediately after a successful exchange.
	db.Exec(`DELETE FROM auth_codes WHERE code = ?`, code) //nolint:errcheck
	return username, scope, nil
}

// ── Bearer token management ───────────────────────────────────────────────────

// tokenTTL is how long a Bearer token remains valid after creation.
const tokenTTL = "-90 days"

// CreateToken generates a random Bearer token for the given user, recording the
// originating client and granted scope.
func CreateToken(db *sql.DB, username, clientID, scope string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	token := hex.EncodeToString(b)
	_, err := db.Exec(
		`INSERT INTO tokens (token, username, client_id, scope) VALUES (?, ?, ?, ?)`,
		token, username, clientID, scope,
	)
	if err != nil {
		return "", fmt.Errorf("storing token: %w", err)
	}
	return token, nil
}

// ValidateToken returns the TokenInfo for a valid, non-expired token.
// Returns nil (with no error) if the token is unknown or expired.
func ValidateToken(db *sql.DB, token string) (*TokenInfo, error) {
	var info TokenInfo
	err := db.QueryRow(`
		SELECT username, client_id, scope FROM tokens
		WHERE token = ? AND created_at > datetime('now', ?)
	`, token, tokenTTL).Scan(&info.Username, &info.ClientID, &info.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// RevokeToken deletes a token from the store, immediately invalidating it.
func RevokeToken(db *sql.DB, token string) error {
	_, err := db.Exec(`DELETE FROM tokens WHERE token = ?`, token)
	return err
}
