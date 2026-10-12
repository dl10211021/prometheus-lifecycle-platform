package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/promlifecycle/platform/services"
	platformsqlite "github.com/promlifecycle/platform/storage/sqlite"
	gormsqlite "gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net/url"
	"os"
	"strconv"
)

func recoverPrometheusRestoreExecutionCommand(ctx context.Context, path, root, idText string) error {
	id, err := strconv.ParseUint(idText, 10, strconv.IntSize)
	if err != nil || id == 0 {
		return errors.New("invalid execution ID")
	}
	path, ownership, err := acquireDatabaseProcessLock(path)
	if err != nil {
		return err
	}
	defer ownership.Close()
	if err := platformsqlite.ValidateRestoreStageRecoveryState(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("existing database required")
	}
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = url.Values{"mode": {"rw"}, "_busy_timeout": {"5000"}}.Encode()
	conn, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	db, err := gorm.Open(gormsqlite.Dialector{Conn: conn}, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return err
	}
	row, err := services.NewBackupServiceWithRoot(db, root).RecoverPrometheusPairedRestoreExecutionForStartup(ctx, uint(id), ownership)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(row)
}
