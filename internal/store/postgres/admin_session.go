package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	adminSessionTTL    = 7 * 24 * time.Hour
	adminSessionMaxAge = 30 * 24 * time.Hour
)

func hashAdminSessionToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func isAdminSessionValid(raw any) bool {
	ts, ok := toFloat(raw)
	if !ok || ts <= 0 {
		return false
	}
	if ts > 1e12 {
		ts = ts / 1000
	}
	age := time.Since(time.Unix(int64(ts), 0))
	return age <= adminSessionTTL && age <= adminSessionMaxAge
}

func (c *Connector) mutateAdminSessions(ctx context.Context, createIfMissing bool, mutate func(sessions map[string]any) bool) error {
	if c == nil || c.Pool == nil {
		return errors.New("postgres admin session store unavailable")
	}
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if createIfMissing {
		if _, err := tx.Exec(ctx, `
			INSERT INTO app_settings (key, value, updated_at)
			VALUES ('sessions', '{}'::jsonb, now())
			ON CONFLICT (key) DO NOTHING
		`); err != nil {
			return err
		}
	}

	var data []byte
	err = tx.QueryRow(ctx, "SELECT value FROM app_settings WHERE key = 'sessions' FOR UPDATE").Scan(&data)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && !createIfMissing {
			return nil
		}
		return err
	}

	sessions := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &sessions)
		if sessions == nil {
			sessions = map[string]any{}
		}
	}

	if !mutate(sessions) {
		return nil
	}

	encoded, err := json.Marshal(sessions)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ('sessions', $1::jsonb, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
	`, encoded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *Connector) VerifyAdminSession(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil || c.Pool == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var data []byte
	if err := c.Pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key = 'sessions'").Scan(&data); err != nil {
		return false
	}
	var sessions map[string]any
	if err := json.Unmarshal(data, &sessions); err != nil || sessions == nil {
		return false
	}
	hashed := hashAdminSessionToken(token)
	raw, ok := sessions[hashed]
	matchedLegacy := false
	if !ok {
		raw, ok = sessions[token]
		if !ok {
			return false
		}
		matchedLegacy = true
	}
	if !isAdminSessionValid(raw) {
		_ = c.DeleteAdminSession(token)
		return false
	}
	if matchedLegacy && hashed != token {
		_ = c.mutateAdminSessions(ctx, false, func(s map[string]any) bool {
			legacyVal, hasPlain := s[token]
			if !hasPlain {
				return false
			}
			if _, hasHashed := s[hashed]; !hasHashed {
				s[hashed] = legacyVal
			}
			delete(s, token)
			return true
		})
	}
	return true
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	case map[string]any:
		for _, k := range []string{"ts", "created_at"} {
			if raw, ok := v[k]; ok {
				if f, ok := toFloat(raw); ok {
					return f, true
				}
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

func (c *Connector) CreateAdminSession(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || c == nil || c.Pool == nil {
		return errors.New("postgres admin session store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hashed := hashAdminSessionToken(token)
	now := float64(time.Now().Unix())
	return c.mutateAdminSessions(ctx, true, func(sessions map[string]any) bool {
		// prune expired
		for k, v := range sessions {
			if !isAdminSessionValid(v) {
				delete(sessions, k)
			}
		}
		delete(sessions, token)
		sessions[hashed] = now
		return true
	})
}

func (c *Connector) DeleteAdminSession(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || c == nil || c.Pool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hashed := hashAdminSessionToken(token)
	return c.mutateAdminSessions(ctx, false, func(sessions map[string]any) bool {
		_, hasHashed := sessions[hashed]
		_, hasPlain := sessions[token]
		if !hasHashed && !hasPlain {
			return false
		}
		delete(sessions, hashed)
		delete(sessions, token)
		return true
	})
}
