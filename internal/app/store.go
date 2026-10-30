package app

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Store struct{ Pool *pgxpool.Pool }
type User struct {
	ID                   int64
	Username, Role, CSRF string
}
type Plan struct {
	ID             int64
	Name, Source   string
	Interval, Keep int
	Enabled        bool
	NextRun        time.Time
}
type Task struct {
	ID                                                           int64
	PlanID                                                       *int64
	PlanName, Source, Kind, Status, SnapshotID, RestorePath, Log string
	Keep                                                         int
	CreatedAt                                                    time.Time
	StartedAt, FinishedAt                                        *time.Time
}
type Snapshot struct {
	ID        string
	PlanID    int64
	PlanName  string
	CreatedAt time.Time
}
type Audit struct {
	Username, Action string
	CreatedAt        time.Time
}

func OpenStore(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	ping, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err = pool.Ping(ping); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}
func (s *Store) Close() { s.Pool.Close() }
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(724100)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Bootstrap(ctx context.Context, cfg Config) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(724102)"); err != nil {
		return err
	}
	for _, item := range []struct{ name, role, password string }{{"admin", "admin", cfg.AdminPassword}, {"viewer", "viewer", cfg.ViewerPassword}} {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)", item.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		hash, err := HashPassword(item.password)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO users(username,role,password_hash) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", item.name, item.role, hash); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO plans(name,source,interval_minutes,keep_last,enabled)
	 SELECT 'Документы компании','documents',60,5,false WHERE NOT EXISTS(SELECT 1 FROM plans)`)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id,name,source,interval_minutes,keep_last,enabled,next_run FROM plans ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Plan
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.ID, &p.Name, &p.Source, &p.Interval, &p.Keep, &p.Enabled, &p.NextRun); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}
func (s *Store) Tasks(ctx context.Context) ([]Task, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,plan_id,plan_name,source,keep_last,kind,status,snapshot_id,restore_path,log,created_at,started_at,finished_at FROM tasks ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.PlanID, &t.PlanName, &t.Source, &t.Keep, &t.Kind, &t.Status, &t.SnapshotID, &t.RestorePath, &t.Log, &t.CreatedAt, &t.StartedAt, &t.FinishedAt); err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}
func (s *Store) Snapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.Pool.Query(ctx, `SELECT s.id,s.plan_id,p.name,s.created_at FROM snapshots s JOIN plans p ON p.id=s.plan_id ORDER BY s.created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Snapshot
	for rows.Next() {
		var p Snapshot
		if err := rows.Scan(&p.ID, &p.PlanID, &p.PlanName, &p.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}
func (s *Store) Audits(ctx context.Context) ([]Audit, error) {
	rows, err := s.Pool.Query(ctx, `SELECT username,action,created_at FROM audit ORDER BY id DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Audit
	for rows.Next() {
		var a Audit
		if err := rows.Scan(&a.Username, &a.Action, &a.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
func auditTx(ctx context.Context, tx pgx.Tx, username, action string) error {
	_, err := tx.Exec(ctx, "INSERT INTO audit(username,action) VALUES($1,$2)", username, action)
	return err
}
func (s *Store) SavePlan(ctx context.Context, user User, p Plan) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if p.ID == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO plans(name,source,interval_minutes,keep_last,enabled,next_run) VALUES($1,$2,$3,$4,$5,now()+$3*interval '1 minute') RETURNING id`, p.Name, p.Source, p.Interval, p.Keep, p.Enabled).Scan(&p.ID)
	} else {
		// Existing snapshots are tied to a source. Source changes require a new plan.
		var result int64
		err = tx.QueryRow(ctx, `UPDATE plans SET name=$2,interval_minutes=$3,keep_last=$4,enabled=$5,next_run=now()+$3*interval '1 minute' WHERE id=$1 AND source=$6 RETURNING id`, p.ID, p.Name, p.Interval, p.Keep, p.Enabled, p.Source).Scan(&result)
	}
	if err != nil {
		return err
	}
	if err = auditTx(ctx, tx, user.Username, fmt.Sprintf("Сохранено задание №%d: %s", p.ID, p.Name)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) DeletePlan(ctx context.Context, user User, id int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var name string
	if err = tx.QueryRow(ctx, "SELECT name FROM plans WHERE id=$1 FOR UPDATE", id).Scan(&name); err != nil {
		return err
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE plan_id=$1 AND status IN ('queued','running'))", id).Scan(&active); err != nil {
		return err
	}
	if active {
		return errors.New("сначала дождитесь завершения операций задания")
	}
	if _, err = tx.Exec(ctx, "DELETE FROM plans WHERE id=$1", id); err != nil {
		return err
	}
	if err = auditTx(ctx, tx, user.Username, fmt.Sprintf("Удалено задание №%d: %s. Снимки в репозитории сохранены", id, name)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Enqueue(ctx context.Context, user User, planID int64, kind, snapshotID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var p Plan
	if err = tx.QueryRow(ctx, "SELECT id,name,source,keep_last FROM plans WHERE id=$1 FOR UPDATE", planID).Scan(&p.ID, &p.Name, &p.Source, &p.Keep); err != nil {
		return err
	}
	if kind == "restore" {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM snapshots WHERE id=$1 AND plan_id=$2)", snapshotID, planID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errors.New("снимок не принадлежит выбранному заданию")
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO tasks(plan_id,plan_name,source,keep_last,kind,snapshot_id) VALUES($1,$2,$3,$4,$5,$6)", p.ID, p.Name, p.Source, p.Keep, kind, snapshotID)
	if err != nil {
		return fmt.Errorf("задание уже выполняется или не удалось добавить операцию: %w", err)
	}
	if err = auditTx(ctx, tx, user.Username, fmt.Sprintf("Операция %s для задания №%d", kind, planID)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
