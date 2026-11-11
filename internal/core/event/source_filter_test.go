package event_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func TestListEventsSourceFilterBeforePagination(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	core := event.NewCore(eventdb.NewDB(db).AutoMigrate(true), nil)
	for i, model := range []string{"GB28181", "yolo", "GB28181", "yolo"} {
		if _, err := core.CreateEvent(t.Context(), &event.AddEventInput{DID: "test", Model: model, Label: "motion", StartedAt: orm.Time{Time: time.Now().Add(time.Duration(i) * time.Second)}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{"alarm", "ai"} {
		first, total, err := core.ListEvents(t.Context(), &event.FindEventInput{Source: source, PagerFilter: web.PagerFilter{Page: 1, Size: 1}})
		if err != nil || total != 2 || len(first) != 1 {
			t.Fatalf("%s: total=%d len=%d err=%v", source, total, len(first), err)
		}
		second, _, err := core.ListEvents(t.Context(), &event.FindEventInput{Source: source, PagerFilter: web.PagerFilter{Page: 2, Size: 1}})
		if err != nil || len(second) != 1 || first[0].ID == second[0].ID {
			t.Fatal("source pagination failed", err)
		}
		for _, e := range append(first, second...) {
			if (e.Model == "GB28181") != (source == "alarm") {
				t.Fatalf("wrong source: %+v", e)
			}
		}
	}
}
