package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	DatabaseURL, Listen, AdminPassword, ViewerPassword                    string
	SourceRoot, RestoreRoot, Repository, RepositoryPassword, ResticBinary string
	CookieSecure, StageSources                                            bool
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func LoadConfig() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"), Listen: env("LISTEN_ADDR", ":8080"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"), ViewerPassword: os.Getenv("VIEWER_PASSWORD"),
		SourceRoot: env("SOURCE_ROOT", "/sources"), RestoreRoot: env("RESTORE_ROOT", "/restores"),
		Repository:         env("RESTIC_REPOSITORY", "rest:http://storage:8000/coursework/"),
		RepositoryPassword: os.Getenv("RESTIC_PASSWORD"), ResticBinary: env("RESTIC_BINARY", "restic"),
		CookieSecure: strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"),
		StageSources: strings.EqualFold(os.Getenv("STAGE_SOURCES"), "true"),
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.AdminPassword) < 12 || len(c.ViewerPassword) < 12 {
		return c, errors.New("ADMIN_PASSWORD and VIEWER_PASSWORD must each contain at least 12 characters")
	}
	if c.AdminPassword == c.ViewerPassword {
		return c, errors.New("admin and viewer passwords must differ")
	}
	if len(c.RepositoryPassword) < 16 {
		return c, errors.New("RESTIC_PASSWORD must contain at least 16 characters")
	}
	var err error
	c.SourceRoot, err = filepath.Abs(c.SourceRoot)
	if err != nil {
		return c, err
	}
	c.RestoreRoot, err = filepath.Abs(c.RestoreRoot)
	return c, err
}
