package ipccache

import (
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"gorm.io/gorm"
)

func TestGBTaskStatePersistsAcrossCacheRestartAndCleansUp(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	base := ipcdb.NewDB(db).AutoMigrate(true)
	first := NewCache(base)
	now := time.Now()
	deviceID := "34020000001320000001"
	currentSession := "snapshot-session-persist-000000001"
	oldSession := "snapshot-session-expired-000000001"
	if err := first.SaveGBTaskState(t.Context(), "snapshot", deviceID, oldSession, []byte(`{"status":"old"}`), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveGBTaskState(t.Context(), "snapshot", deviceID, currentSession, []byte(`{"status":"pending"}`), now); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveGBTaskState(t.Context(), "snapshot", deviceID, currentSession, []byte(`{"status":"accepted"}`), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	restarted := NewCache(ipcdb.NewDB(db))
	payload, ok, err := restarted.LoadGBTaskState(t.Context(), "snapshot", deviceID, currentSession)
	if err != nil || !ok || string(payload) != `{"status":"accepted"}` {
		t.Fatalf("restored task state = %s, %v, %v", payload, ok, err)
	}
	if err := restarted.CleanupGBTaskStates(t.Context(), "snapshot", now.Add(-7*24*time.Hour), 1024); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restarted.LoadGBTaskState(t.Context(), "snapshot", deviceID, oldSession); err != nil || ok {
		t.Fatalf("expired task state survived cleanup: ok=%v err=%v", ok, err)
	}
}
