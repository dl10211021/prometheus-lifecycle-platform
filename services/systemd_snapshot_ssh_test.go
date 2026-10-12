package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/promlifecycle/platform/models"
	"github.com/promlifecycle/platform/pkg/utils"
	"net"
	"os"
	"testing"
	"time"
)

// Opt-in SSH transport acceptance. This uses an owned sleep service to check
// remote file/state preservation; real Prometheus runs in the local acceptance.
func TestSSHSystemdUnitSnapshot(t *testing.T) {
	host := os.Getenv("PROMLIFECYCLE_SNAPSHOT_SSH_HOST")
	password := os.Getenv("PROMLIFECYCLE_SNAPSHOT_SSH_PASSWORD")
	if host == "" || password == "" {
		t.Skip("requires explicit SSH snapshot fixture credentials")
	}
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("plc-snapshot-ssh-%x.service", token[:])
	root := fmt.Sprintf("/var/lib/plc-snapshot-ssh-%x", token[:])
	unitPath := "/etc/systemd/system/" + unit
	s, c, _ := snapshotServiceFixture(t)
	if err := s.db.Model(&models.Host{}).Where("id = ?", c.HostID).Updates(map[string]any{"ip": host, "port": 22, "username": "root", "password": password, "connection_type": "ssh"}).Error; err != nil {
		t.Fatal("record fixture host")
	}
	if err := s.db.Model(&models.Component{}).Where("id = ?", c.ID).Updates(map[string]any{"service_name": unit, "imported_args": ""}).Error; err != nil {
		t.Fatal("record fixture component")
	}
	var current models.Component
	if err := s.db.Preload("Host").First(&current, c.ID).Error; err != nil {
		t.Fatal("load fixture identity")
	}
	r, err := s.runnerFor(&current)
	if err != nil {
		t.Fatal("create remote runner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	r, closeFixtureRunner, err := acquireUnitSnapshotRunner(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	// Register before fixture cleanup so LIFO cleanup still has its transport.
	t.Cleanup(closeFixtureRunner)
	content := fmt.Sprintf("[Service]\nType=simple\nEnvironmentFile=%s/protected.env\nEnvironmentFile=-%s/optional.env\nExecStart=/bin/sleep 300\n[Install]\nWantedBy=multi-user.target\n", root, root)
	// Create the exclusive ownership marker first; cleanup only follows a
	// successful exclusive mkdir, never an arbitrary/preexisting unit path.
	_, err = snapshotRun(ctx, r, "set -eu\numask 077\ntest ! -e "+utils.QuoteShell(unitPath)+" && test ! -L "+utils.QuoteShell(unitPath)+"\nmkdir -m 0700 -- "+utils.QuoteShell(root))
	if err != nil {
		t.Fatal("create exclusive remote fixture", err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		cleanupBase, err := s.runnerFor(&current)
		if err != nil {
			t.Error("create cleanup runner", err)
			return
		}
		cleanupRunner, closeCleanup, err := acquireUnitSnapshotRunner(cleanup, cleanupBase)
		if err != nil {
			t.Error("connect owned remote cleanup", err)
			return
		}
		defer closeCleanup()
		// Stop must succeed and report MainPID=0 before removing the owned files.
		cmd := "set -eu\nunit=" + utils.QuoteShell(unit) + "\nroot=" + utils.QuoteShell(root) + "\npath=" + utils.QuoteShell(unitPath) + `
if test -e "$path"; then
 systemctl stop "$unit"
 test "$(systemctl show --property=MainPID --value "$unit")" = 0
 rm -- "$path"
fi
systemctl daemon-reload
test "$(readlink -m -- "$root")" = "$root"
rm -rf -- "$root"
`
		if _, err := snapshotRun(cleanup, cleanupRunner, cmd); err != nil {
			t.Error("owned remote cleanup unconfirmed", err)
		}
	})
	setup := "set -eu\numask 077\nset -C\nprintf %s " + utils.QuoteShell(base64.StdEncoding.EncodeToString([]byte(content))) + " | base64 -d > " + utils.QuoteShell(unitPath) + "\nprintf '%s\\n' 'SSH_SNAPSHOT_SECRET=private-remote-sentinel' > " + utils.QuoteShell(root+"/protected.env") + "\nchmod 0644 " + utils.QuoteShell(unitPath) + "\nsystemctl daemon-reload\nsystemctl start " + utils.QuoteShell(unit)
	if _, err := snapshotRun(ctx, r, setup); err != nil {
		t.Fatal("start owned SSH fixture", err)
	}
	before, _, err := snapshotInventory(ctx, r, unit)
	if err != nil {
		t.Fatal(err)
	}
	s.backupRoot = root + "/snapshots"
	s.runnerFactory = s.runnerFor
	ref, err := s.CaptureComponentUnitSnapshot(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewBackupServiceWithRoot(s.db, s.backupRoot)
	proof, err := fresh.VerifyComponentUnitSnapshot(ctx, ref, true)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := snapshotInventory(ctx, r, unit)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Files != 3 || proof.PresentFiles != 2 || before["MainPID"] != after["MainPID"] || before["UnitFileState"] != after["UnitFileState"] {
		t.Fatal("SSH snapshot changed runtime or lost files")
	}
	t.Logf("SSH transport verified: files=%d present=%d PID and enablement unchanged", proof.Files, proof.PresentFiles)
}

func TestUnitSnapshotSSHConnectionDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := remoteBackupRunner{component: &models.Component{Host: models.Host{IP: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Username: "root", Password: "fixture-only"}}}
	_, release, err := acquireUnitSnapshotRunner(ctx, r)
	if release != nil {
		release()
	}
	select {
	case conn := <-accepted:
		conn.Close()
	default:
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("snapshot SSH dial ignored deadline: %v", err)
	}
}
