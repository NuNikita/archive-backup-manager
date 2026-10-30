package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func RunWorker(ctx context.Context, s *Store, cfg Config) error {
	e := Engine{Config: cfg}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	slog.Info("backup worker started")
	for {
		if err := workerTick(ctx, s, e); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("worker iteration", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func workerTick(ctx context.Context, s *Store, e Engine) error {
	// Hold a dedicated connection: all workers share one repository lock.
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(724101)").Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var ok bool
		if err := conn.QueryRow(unlock, "SELECT pg_advisory_unlock(724101)").Scan(&ok); err != nil {
			// Never return a connection with an unknown session lock to the pool.
			_ = conn.Conn().Close(unlock)
		}
	}()
	// A running row with this exclusive lock acquired means the previous worker stopped.
	if _, err = conn.Exec(ctx, `UPDATE tasks SET status='failed',finished_at=now(),log=log||E'\nИсполнитель был остановлен; проверьте репозиторий перед повторным запуском.' WHERE status='running'`); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `WITH due AS (
	 SELECT p.id FROM plans p WHERE p.enabled AND p.next_run<=now()
	 AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.plan_id=p.id AND t.status IN ('queued','running'))
	 ORDER BY p.id FOR UPDATE SKIP LOCKED
	), added AS (
	 INSERT INTO tasks(plan_id,plan_name,source,keep_last,kind)
	 SELECT p.id,p.name,p.source,p.keep_last,'backup' FROM plans p JOIN due d ON d.id=p.id RETURNING plan_id
	) UPDATE plans p SET next_run=now()+p.interval_minutes*interval '1 minute' FROM added a WHERE p.id=a.plan_id`)
	if err != nil {
		return err
	}
	var t Task
	err = tx.QueryRow(ctx, `UPDATE tasks SET status='running',started_at=now() WHERE id=(SELECT id FROM tasks WHERE status='queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
	 RETURNING id,plan_id,plan_name,source,keep_last,kind,snapshot_id`).Scan(&t.ID, &t.PlanID, &t.PlanName, &t.Source, &t.Keep, &t.Kind, &t.SnapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	op, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	var output, dest, snapshot string
	err = e.Init(op)
	if err == nil {
		switch t.Kind {
		case "backup":
			snapshot, output, err = e.Backup(op, t)
		case "restore":
			dest, output, err = e.Restore(op, t)
		case "check":
			output, err = e.Check(op)
		default:
			err = fmt.Errorf("unknown task kind: %s", t.Kind)
		}
	}
	// Record a successful snapshot before pruning so it stays visible if retention fails.
	if snapshot != "" && t.PlanID != nil {
		if _, saveErr := conn.Exec(op, "INSERT INTO snapshots(id,plan_id) VALUES($1,$2) ON CONFLICT DO NOTHING", snapshot, *t.PlanID); saveErr != nil {
			err = saveErr
		}
		if err == nil {
			retentionLog, retentionErr := e.Retain(op, t)
			output += "\n" + retentionLog
			err = retentionErr
			if err == nil {
				ids, listErr := e.SnapshotIDs(op, *t.PlanID)
				if listErr != nil {
					err = listErr
				} else {
					_, err = conn.Exec(op, "DELETE FROM snapshots WHERE plan_id=$1 AND NOT(id=ANY($2::text[]))", *t.PlanID, ids)
				}
			}
		}
	}
	status := "success"
	if err != nil {
		status = "failed"
		output += "\n" + err.Error()
	}
	if len(output) > 256*1024 {
		output = output[:256*1024]
	}
	output = strings.ToValidUTF8(output, "�")
	if snapshot == "" {
		snapshot = t.SnapshotID
	}
	finish, cancelFinish := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFinish()
	_, saveErr := conn.Exec(finish, "UPDATE tasks SET status=$2,snapshot_id=$3,restore_path=$4,log=$5,finished_at=now() WHERE id=$1", t.ID, status, snapshot, dest, output)
	slog.Info("task finished", "id", t.ID, "status", status)
	return saveErr
}
