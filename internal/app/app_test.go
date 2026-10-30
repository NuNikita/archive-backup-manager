package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	hash, err := HashPassword("long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "long-test-password") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Fatal("incorrect password accepted")
	}
	if VerifyPassword("pbkdf2-sha256$bad$bad", "anything") {
		t.Fatal("malformed hash accepted")
	}
	other, _ := HashPassword("long-test-password")
	if hash == other {
		t.Fatal("password salts are not random")
	}
}

func TestSourceConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "documents"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"../outside", "/etc", ".", "-option", "escape", "missing", "documents/../..", "foo\\bar"} {
		if _, err := ValidateSource(root, source); err == nil {
			t.Errorf("unsafe source accepted: %q", source)
		}
	}
	if got, err := ValidateSource(root, "documents"); err != nil || got != "documents" {
		t.Fatalf("valid source: %q %v", got, err)
	}
}

func engineFixture(t *testing.T) (Engine, string) {
	t.Helper()
	binary, err := exec.LookPath(env("RESTIC_BINARY", "restic"))
	if err != nil {
		t.Skip("restic executable unavailable; install it for the round trip test")
	}
	root := t.TempDir()
	source := filepath.Join(root, "sources")
	if err := os.MkdirAll(filepath.Join(source, "documents"), 0755); err != nil {
		t.Fatal(err)
	}
	e := Engine{Config: Config{SourceRoot: source, RestoreRoot: filepath.Join(root, "restored"), Repository: filepath.Join(root, "repository"), RepositoryPassword: "test-only-repository-password", ResticBinary: binary}}
	return e, filepath.Join(source, "documents", "report.txt")
}

func TestResticRoundTrip(t *testing.T) {
	testResticRoundTrip(t, false)
}

func TestStagedResticRoundTrip(t *testing.T) {
	testResticRoundTrip(t, true)
}

func testResticRoundTrip(t *testing.T, stage bool) {
	e, file := engineFixture(t)
	e.Config.StageSources = stage
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := os.WriteFile(file, []byte("version one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	id := int64(1)
	task := Task{ID: 1, PlanID: &id, Source: "documents", Keep: 1}
	first, log, err := e.Backup(ctx, task)
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, log)
	}
	if err := os.WriteFile(file, []byte("version two is different\n"), 0644); err != nil {
		t.Fatal(err)
	}
	second, log, err := e.Backup(ctx, task)
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, log)
	}
	if first == second {
		t.Fatal("changed data produced identical snapshots")
	}
	task.SnapshotID = first
	dest, log, err := e.Restore(ctx, task)
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, log)
	}
	found := false
	err = filepath.WalkDir(dest, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == "report.txt" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(data) != "version one\n" {
				t.Errorf("wrong recovered data: %q", data)
			}
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("recovered file missing")
	}
	if log, err = e.Retain(ctx, task); err != nil {
		t.Fatalf("retention: %v\n%s", err, log)
	}
	ids, err := e.SnapshotIDs(ctx, id)
	if err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatalf("retention IDs: %v, %v", ids, err)
	}
	if log, err = e.Check(ctx); err != nil {
		t.Fatalf("integrity: %v\n%s", err, log)
	}
}

func TestHTTPGuards(t *testing.T) {
	// No database is needed for a request rejected before authentication.
	handler := NewServer(nil, Config{})
	r := httptest.NewRequest("POST", "http://archive.local/login", strings.NewReader("username=admin"))
	r.Header.Set("Origin", "http://attacker.local")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "http://archive.local/login", strings.NewReader("username=admin"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing csrf accepted: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "http://archive.local/login", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Данные под контролем") {
		t.Fatal("login template failed")
	}
	if len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].HttpOnly {
		t.Fatal("missing protected login cookie")
	}
}

func integrationStore(t *testing.T) (*Store, Config) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL unavailable; database integration tests run in CI")
	}
	ctx := context.Background()
	admin, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "test_" + tokenHash(token())[:12]
	if _, err = admin.Pool.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Pool.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE"); admin.Close() })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", name)
	parsed.RawQuery = query.Encode()
	s, err := OpenStore(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AdminPassword: "integration-admin-password", ViewerPassword: "integration-viewer-password"}
	if err = s.Bootstrap(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return s, cfg
}

func TestDatabaseQueueAndPermissions(t *testing.T) {
	s, cfg := integrationStore(t)
	ctx := context.Background()
	adminToken, err := s.Login(ctx, "admin", cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Session(ctx, adminToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Login(ctx, "admin", "wrong"); err == nil {
		t.Fatal("invalid login accepted")
	}
	plans, err := s.Plans(ctx)
	if err != nil || len(plans) != 1 {
		t.Fatalf("bootstrap plans: %v %v", plans, err)
	}
	id := plans[0].ID
	if err = s.Enqueue(ctx, u, id, "backup", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, u, id, "backup", ""); err == nil {
		t.Fatal("duplicate pending task accepted")
	}
	if err = s.DeletePlan(ctx, u, id); err == nil {
		t.Fatal("plan with pending task deleted")
	}
	if err = s.Enqueue(ctx, u, id, "restore", strings.Repeat("a", 64)); err == nil {
		t.Fatal("unregistered snapshot accepted")
	}
	viewerToken, err := s.Login(ctx, "viewer", cfg.ViewerPassword)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := s.Session(ctx, viewerToken)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SourceRoot = t.TempDir()
	handler := NewServer(s, cfg)
	values := url.Values{"csrf": {viewer.CSRF}, "id": {fmtID(id)}}
	r := httptest.NewRequest("POST", "http://archive.local/tasks/backup", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: viewerToken})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("viewer can mutate: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "http://archive.local/", nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: adminToken})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("Документы компании")) {
		t.Fatal("authenticated dashboard failed")
	}
	if strings.Contains(w.Body.String(), "ZgotmplZ") {
		t.Fatal("invalid template context")
	}
}

func TestConcurrentBootstrap(t *testing.T) {
	s, cfg := integrationStore(t)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, "TRUNCATE users, plans CASCADE"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errors <- s.Bootstrap(ctx, cfg) })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var users, plans int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM plans").Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if users != 2 || plans != 1 {
		t.Fatalf("concurrent initialization duplicated data: users=%d plans=%d", users, plans)
	}
}

func TestWorkerDatabaseRoundTrip(t *testing.T) {
	s, cfg := integrationStore(t)
	e, file := engineFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(file, []byte("original corporate data\n"), 0644); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Login(ctx, "admin", cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Session(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := s.Plans(ctx)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans: %v %v", plans, err)
	}
	id := plans[0].ID
	if err = s.Enqueue(ctx, u, id, "backup", ""); err != nil {
		t.Fatal(err)
	}
	if err = workerTick(ctx, s, e); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Tasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].Status != "success" {
		t.Fatalf("backup task: %+v %v", tasks, err)
	}
	snaps, err := s.Snapshots(ctx)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, u, id, "restore", snaps[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = workerTick(ctx, s, e); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.Tasks(ctx)
	if err != nil || tasks[0].Status != "success" || tasks[0].RestorePath == "" {
		t.Fatalf("restore task: %+v %v", tasks, err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("restore overwrote original source")
	}
	// Overdue schedules are queued once and rescheduled after the current time.
	if err = os.WriteFile(file, []byte("new corporate data\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool.Exec(ctx, "UPDATE plans SET enabled=true,next_run=now()-interval '1 minute' WHERE id=$1", id)
	if err != nil {
		t.Fatal(err)
	}
	if err = workerTick(ctx, s, e); err != nil {
		t.Fatal(err)
	}
	if err = workerTick(ctx, s, e); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.Tasks(ctx)
	if err != nil || len(tasks) != 3 || tasks[0].Status != "success" {
		t.Fatalf("scheduled tasks: %+v %v", tasks, err)
	}
}

func fmtID(id int64) string { return strconv.FormatInt(id, 10) }
