package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDatabaseProcessLockLifetimeBindingAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	canonical, lock, err := AcquireDatabaseProcessLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, other, err := AcquireDatabaseProcessLock(path); err == nil {
		other.Close()
		t.Fatal("duplicate ownership")
	}
	if err := lock.WithOwnership(context.Background(), canonical+"wrong", func() error { t.Fatal("wrong callback"); return nil }); err == nil {
		t.Fatal("wrong database accepted")
	}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- lock.WithOwnership(context.Background(), canonical, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lock.WithOwnership(ctx, canonical, func() error { t.Fatal("cancelled callback"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- lock.Close() }()
	select {
	case <-closed:
		t.Fatal("Close released callback ownership")
	case <-time.After(30 * time.Millisecond):
	}
	if _, other, err := AcquireDatabaseProcessLock(path); err == nil {
		other.Close()
		t.Fatal("lock lost during callback")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := lock.WithOwnership(context.Background(), canonical, func() error { t.Fatal("closed callback"); return nil }); err == nil {
		t.Fatal("closed accepted")
	}
}
func TestDatabaseProcessLockRejectsAliasesAndReplacedLock(t *testing.T) {
	for _, mode := range []string{"database-hardlink", "lock-symlink", "lock-hardlink", "lock-replaced"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			os.WriteFile(path, []byte("existing"), 0600)
			switch mode {
			case "database-hardlink":
				os.Link(path, path+"alias")
			case "lock-symlink":
				os.WriteFile(path+"target", nil, 0600)
				os.Symlink(path+"target", path+".lock")
			case "lock-hardlink":
				os.WriteFile(path+"target", nil, 0600)
				os.Link(path+"target", path+".lock")
			}
			canonical, lock, err := AcquireDatabaseProcessLock(path)
			if mode != "lock-replaced" {
				if err == nil {
					lock.Close()
					t.Fatal("unsafe alias accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			os.Rename(path+".lock", path+".old")
			os.WriteFile(path+".lock", nil, 0600)
			if err := lock.WithOwnership(context.Background(), canonical, func() error { t.Fatal("replaced lock callback"); return nil }); err == nil {
				t.Fatal("replaced accepted")
			}
		})
	}
}
