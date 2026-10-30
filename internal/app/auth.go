package app

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const passwordIterations = 600000

func token() string { return rand.Text() }
func tokenHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%x$%x", salt, key), nil
}
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	salt, err := hex.DecodeString(parts[1])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := hex.DecodeString(parts[2])
	if err != nil || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	return err == nil && subtle.ConstantTimeCompare(want, got) == 1
}
func (s *Store) Login(ctx context.Context, username, password string) (string, error) {
	var id int64
	var hash string
	err := s.Pool.QueryRow(ctx, "SELECT id,password_hash FROM users WHERE username=$1", username).Scan(&id, &hash)
	if err != nil {
		// Same password derivation cost for an unknown username.
		_, _ = pbkdf2.Key(sha256.New, password, make([]byte, 16), passwordIterations, 32)
		return "", fmt.Errorf("неверное имя пользователя или пароль")
	}
	if !VerifyPassword(hash, password) {
		return "", fmt.Errorf("неверное имя пользователя или пароль")
	}
	raw := token()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "DELETE FROM sessions WHERE expires_at < now()"); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO sessions(token_hash,user_id,csrf_token,expires_at) VALUES($1,$2,$3,$4)", tokenHash(raw), id, token(), time.Now().Add(12*time.Hour)); err != nil {
		return "", err
	}
	if err = auditTx(ctx, tx, username, "Вход в систему"); err != nil {
		return "", err
	}
	return raw, tx.Commit(ctx)
}
func (s *Store) Session(ctx context.Context, raw string) (User, error) {
	var u User
	err := s.Pool.QueryRow(ctx, `SELECT u.id,u.username,u.role,s.csrf_token FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now()`, tokenHash(raw)).Scan(&u.ID, &u.Username, &u.Role, &u.CSRF)
	return u, err
}
