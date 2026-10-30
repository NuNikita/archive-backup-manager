package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var snapshotPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidateSource confines configurable sources to an administrator-mounted directory.
func ValidateSource(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || strings.ContainsRune(relative, 0) {
		return "", errors.New("укажите относительную папку внутри sources")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.HasPrefix(clean, "-") {
		return "", errors.New("недопустимый путь источника")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(abs, clean))
	if err != nil {
		return "", fmt.Errorf("папка источника недоступна: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("источник находится вне разрешённой папки")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("источник должен быть папкой")
	}
	return clean, nil
}

// boundedWriter drains command output while retaining only a bounded prefix.
type boundedWriter struct {
	bytes.Buffer
	limit int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.Buffer.Write(p)
	}
	return n, nil
}

type Engine struct{ Config Config }

func (e Engine) command(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.Config.ResticBinary, args...)
	cmd.Dir = e.Config.SourceRoot
	cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+e.Config.Repository, "RESTIC_PASSWORD="+e.Config.RepositoryPassword, "RESTIC_CACHE_DIR="+filepath.Join(os.TempDir(), "backup-manager-restic-cache"))
	out := &boundedWriter{limit: 256 * 1024}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("restic %s: %w", args[0], err)
	}
	return out.String(), nil
}
func (e Engine) Init(ctx context.Context) error {
	if _, err := e.command(ctx, "cat", "config"); err == nil {
		return nil
	}
	output, err := e.command(ctx, "init")
	if err != nil {
		return fmt.Errorf("%w\n%s", err, output)
	}
	return nil
}
func planTag(id int64) string { return "plan-" + strconv.FormatInt(id, 10) }
func (e Engine) Backup(ctx context.Context, t Task) (string, string, error) {
	if t.PlanID == nil {
		return "", "", errors.New("задание удалено")
	}
	source, err := ValidateSource(e.Config.SourceRoot, t.Source)
	if err != nil {
		return "", "", err
	}
	if e.Config.StageSources {
		// Docker Desktop file sharing can accept O_NOATIME yet fail subsequent
		// reads. Stage an independent copy without changing the read-only source.
		stage, err := os.MkdirTemp("", "archive-source-")
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(stage)
		destination := filepath.Join(stage, source)
		if err := os.MkdirAll(filepath.Dir(destination), 0750); err != nil {
			return "", "", err
		}
		copyCommand := exec.CommandContext(ctx, "cp", "-a", filepath.Join(e.Config.SourceRoot, source), destination)
		if output, err := copyCommand.CombinedOutput(); err != nil {
			return "", string(output), fmt.Errorf("подготовка источника: %w", err)
		}
		e.Config.SourceRoot = stage
	}
	output, err := e.command(ctx, "backup", "--json", "--tag", planTag(*t.PlanID), "--", source)
	if err != nil {
		return "", output, err
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	snapshot := ""
	for {
		var event struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := decoder.Decode(&event); err != nil {
			if !errors.Is(err, io.EOF) {
				return "", output, err
			}
			break
		}
		if event.MessageType == "summary" {
			snapshot = event.SnapshotID
		}
	}
	if !snapshotPattern.MatchString(snapshot) {
		return "", output, errors.New("restic не вернул идентификатор снимка")
	}
	return snapshot, output, nil
}
func (e Engine) Retain(ctx context.Context, t Task) (string, error) {
	if t.PlanID == nil {
		return "", errors.New("задание удалено")
	}
	return e.command(ctx, "forget", "--tag", planTag(*t.PlanID), "--group-by", "tags", "--keep-last", strconv.Itoa(t.Keep), "--prune")
}
func (e Engine) SnapshotIDs(ctx context.Context, planID int64) ([]string, error) {
	output, err := e.command(ctx, "snapshots", "--json", "--tag", planTag(planID))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal([]byte(output), &rows); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}
func (e Engine) Restore(ctx context.Context, t Task) (string, string, error) {
	if !snapshotPattern.MatchString(t.SnapshotID) {
		return "", "", errors.New("неверный идентификатор снимка")
	}
	if err := os.MkdirAll(e.Config.RestoreRoot, 0750); err != nil {
		return "", "", err
	}
	// A fresh directory prevents retries from overwriting a previous restore.
	dest, err := os.MkdirTemp(e.Config.RestoreRoot, fmt.Sprintf("task-%d-", t.ID))
	if err != nil {
		return "", "", err
	}
	output, err := e.command(ctx, "restore", t.SnapshotID, "--target", dest, "--verify")
	return dest, output, err
}
func (e Engine) Check(ctx context.Context) (string, error) {
	return e.command(ctx, "check", "--read-data")
}

const taskTimeout = 30 * time.Minute
