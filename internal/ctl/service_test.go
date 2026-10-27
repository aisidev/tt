package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/capability"
	"github.com/aisidev/tt/internal/runcmd"
	"github.com/aisidev/tt/model"
)

func TestServiceShutdownCancelsDetachedCreationAndRetainsItsPlan(t *testing.T) {
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("service integration is opt-in")
	}
	dir := t.TempDir()
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(dir, "ctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	mock := &mockAgent{vms: []model.Vm{}, caps: append([]string{}, capability.LegacyAll...), resource: model.Resource{CPUTotal: 4, MemTotal: 4096, DiskTotal: 1024}}
	mock.beforeCreate = func(request api.CreateVmReq) { enterOnce.Do(func() { close(entered) }); <-release }
	peer := httptest.NewServer(mock.handler())
	defer peer.Close()
	host := hostRecord("host")
	host.Addr = strings.TrimPrefix(peer.URL, "http://")
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- serveController(running, Config{Listen: address, DataDir: dir}, io.Discard) }()
	defer releaseOnce.Do(func() { close(release) })
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/api/status")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("controller did not start", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := json.Marshal(environmentRequest("drained"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post("http://"+address+"/api/envs", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatal(response.StatusCode)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("agent create not entered")
	}
	if lock, err := runcmd.LockState(filepath.Join(dir, "service.lock")); err == nil {
		lock.Close()
		t.Fatal("running controller did not retain the state lock")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for a detached agent request")
	}
	db, err = OpenDB(ctx, filepath.Join(dir, "ctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	env, err := db.GetEnv(ctx, "drained")
	if err != nil || env == nil || env.State != model.EnvCreating {
		t.Fatal(env, err)
	}
	vms, err := db.VMsByEnv(ctx, "drained")
	if err != nil || len(vms) != 1 || vms[0].State != model.VmCreating || vms[0].Error != nil {
		t.Fatal(vms, err)
	}
	lock, err := runcmd.LockState(filepath.Join(dir, "service.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	// The remote agent may finish after the controller leaves. A new controller
	// must recover that result without losing or duplicating the original plan.
	releaseOnce.Do(func() { close(release) })
	deadline = time.Now().Add(5 * time.Second)
	for mock.creates.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("mock agent did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	recovered := newServer(db, nil)
	reader, err := recovered.clientFactory(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	recovered.RefreshAllHosts(ctx, reader)
	env, err = db.GetEnv(ctx, "drained")
	if err != nil || env == nil || env.State != model.EnvActive {
		t.Fatal(env, err)
	}
}

func TestServiceShutdownDrainsForegroundDeletionBeforeCancellingWork(t *testing.T) {
	if os.Getenv("TT_TEST_INTEGRATION") != "1" {
		t.Skip("service integration is opt-in")
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := OpenDB(ctx, filepath.Join(dir, "ctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	vm := vmRecord("vm", "env", "host")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	mock := &mockAgent{vms: []model.Vm{vm}, caps: append([]string{}, capability.LegacyAll...), resource: model.Resource{CPUTotal: 4, MemTotal: 4096, DiskTotal: 1024}, beforeDelete: func() { close(entered); <-release }}
	peer := httptest.NewServer(mock.handler())
	defer peer.Close()
	defer once.Do(func() { close(release) })
	host := hostRecord("host")
	host.Addr = strings.TrimPrefix(peer.URL, "http://")
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	env := model.Env{ID: "env", VmIds: model.List[string]{"vm"}, State: model.EnvActive}
	if err := db.PutEnvironment(ctx, &env, []model.Vm{vm}, nil); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- serveController(running, Config{Listen: address, DataDir: dir}, io.Discard) }()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/api/status")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("controller did not start", err)
		}
		time.Sleep(time.Millisecond)
	}
	responseDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest("DELETE", "http://"+address+"/api/envs/env", nil)
		response, err := client.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				err = fmt.Errorf("status %d", response.StatusCode)
			}
		}
		responseDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delete did not enter agent")
	}
	cancel()
	select {
	case code := <-done:
		t.Fatal("shutdown abandoned an active HTTP handler", code)
	case <-time.After(100 * time.Millisecond):
	}
	if lock, err := runcmd.LockState(filepath.Join(dir, "service.lock")); err == nil {
		lock.Close()
		t.Fatal("lock released during foreground deletion")
	}
	once.Do(func() { close(release) })
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	db, err = OpenDB(ctx, filepath.Join(dir, "ctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	retained, err := db.GetEnv(ctx, "env")
	if err != nil || retained != nil {
		t.Fatal(retained, err)
	}
}

func TestRefreshReplacesUndecodableRowsWithOfflineMarkers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := hostRecord("host-good")
	if err := db.PutHost(ctx, &host); err != nil {
		t.Fatal(err)
	}
	if _, err := db.q.ExecContext(ctx, "INSERT INTO hosts (id,data) VALUES ('host-bad', ?1)", "{broken json"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.q.ExecContext(ctx, "INSERT INTO envs (id,data) VALUES ('env-bad', ?1)", "{broken json"); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	db.stderr = &logs
	s := newServer(db, nil)
	client, err := s.clientFactory(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.refreshHosts(ctx, client, nil)
	if t.Failed() || logs.Len() > 0 {
		t.Logf("refresh logs: %s", logs.String())
	}
	bad, err := db.GetHost(ctx, "host-bad")
	if err != nil {
		t.Fatal(err)
	}
	if bad == nil || bad.State != model.HostOffline || bad.Error == nil || !strings.Contains(*bad.Error, "offline marker") {
		t.Fatal(bad)
	}
	envBad, err := db.GetEnv(ctx, "env-bad")
	if err != nil {
		t.Fatal(err)
	}
	if envBad == nil || envBad.State != model.EnvFailed || envBad.Error == nil || !strings.Contains(*envBad.Error, "marker") {
		t.Fatal(envBad)
	}
	good, err := db.GetHost(ctx, "host-good")
	if err != nil || good == nil || good.Addr != "host-good:9100" {
		t.Fatal(good, err)
	}
}
