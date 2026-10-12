package services

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	coressh "github.com/promlifecycle/platform/core/ssh"
	"github.com/promlifecycle/platform/models"
	gossh "golang.org/x/crypto/ssh"
)

func TestChangeFailureQuarantinesUnknownRemoteTermination(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			db := lifecycleQueueDB(t)
			if err := db.AutoMigrate(&models.Backup{}, &models.Cluster{}, &models.Host{}); err != nil {
				t.Fatal(err)
			}
			component := models.Component{Name: "mutation", DeployType: "target", Version: "new", Status: models.ComponentStatus("removed")}
			plan := models.ChangePlan{Kind: models.ChangeKindRemove, After: []byte(`{"mode":"keep_service"}`), Status: models.ChangeStatusExecuting}
			op := models.Operation{Type: "upgrade", Status: "running"}
			if err := db.Create(&component).Error; err != nil {
				t.Fatal(err)
			}
			plan.ResourceID = component.ID
			snapshot := component
			snapshot.Status = models.StatusRunning
			plan.Before, _ = json.Marshal(componentRemovalSnapshot{Component: snapshot})
			db.Create(&plan)
			db.Create(&op)
			ctx, release, err := ClaimComponentOperation(context.Background(), db, component.ID, "upgrade")
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("confirmed command exit 1")
			if unknown {
				cause = fmt.Errorf("remove remote data: %w", coressh.ErrRemoteTerminationUnconfirmed)
			}
			service := &ChangeService{db: db, backups: NewBackupServiceWithRoot(db, t.TempDir())}
			_, err = service.rollbackAfterFailure(ctx, &plan, nil, &component, &op, "apply", cause)
			release()
			if !errors.Is(err, cause) && !errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
				t.Fatalf("lost cause: %v", err)
			}
			db.First(&component, component.ID)
			db.First(&plan, plan.ID)
			if unknown {
				if component.Status != models.ComponentStatus("removed") || plan.Status != models.ChangeStatusFailed {
					t.Fatalf("unsafe compensation changed state: %s/%s", component.Version, plan.Status)
				}
				var pause ComponentBackupPause
				if err := db.First(&pause, "component_id = ?", component.ID).Error; err != nil || pause.Status != "termination_unconfirmed" {
					t.Fatalf("missing durable block: %+v %v", pause, err)
				}
				if err := RecoverInterruptedLifecycle(db); err != nil {
					t.Fatal(err)
				}
				if _, release, err := ClaimComponentOperation(context.Background(), db, component.ID, "retry"); err == nil {
					release()
					t.Fatal("later mutation admitted after unknown termination")
				}
			} else {
				if component.Status != models.StatusRunning || plan.Status != models.ChangeStatusRolledBack {
					t.Fatalf("confirmed failure not recovered: %s/%s", component.Version, plan.Status)
				}
				_, release, err := ClaimComponentOperation(context.Background(), db, component.ID, "retry")
				if err != nil {
					t.Fatalf("confirmed failure unnecessarily blocked: %v", err)
				}
				release()
			}
		})
	}
}

func TestRemoteDataRemovalPropagatesUnknownTermination(t *testing.T) {
	t.Setenv("PROMLIFECYCLE_SSH_HOST_KEY_MODE", "insecure")
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := gossh.NewSignerFromKey(private)
	config := &gossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	command := make(chan string, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		server, channels, requests, err := gossh.NewServerConn(raw, config)
		if err != nil {
			return
		}
		defer server.Close()
		go gossh.DiscardRequests(requests)
		incoming := <-channels
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		for request := range requests {
			if request.Type == "exec" {
				var payload struct{ Command string }
				gossh.Unmarshal(request.Payload, &payload)
				request.Reply(true, nil)
				command <- payload.Command
				server.Close()
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	component := &models.Component{DataPath: "/var/lib/fixture-data", Host: models.Host{IP: "127.0.0.2", Port: listener.Addr().(*net.TCPAddr).Port, Username: "test", Password: "fixture", ConnectionType: "ssh"}}
	if err := removeComponentData(ctx, component); !errors.Is(err, coressh.ErrRemoteTerminationUnconfirmed) {
		t.Fatalf("remote loss was treated as safe failure: %v", err)
	}
	select {
	case <-command:
	case <-time.After(time.Second):
		t.Fatal("mutation never reached peer")
	}
}
