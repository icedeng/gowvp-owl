package main

import (
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/event"
	"gorm.io/gorm"
	"testing"
)

func TestRecoverySchemaPreservesOldEventsAndReapplies(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = db.Exec("CREATE TABLE events (id integer PRIMARY KEY, label text, image_path text, video_path text)").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO events(id,label,image_path,video_path) VALUES (1,'old','original.jpg','clip.mp4')").Error; err != nil {
		t.Fatal(err)
	}
	if verify(db) == nil {
		t.Fatal("旧结构错误地通过校验")
	}
	for i := 0; i < 2; i++ {
		if err = apply(db); err != nil {
			t.Fatal(err)
		}
	}
	var saved event.Event
	if err = db.First(&saved, 1).Error; err != nil || saved.Historical || saved.Label != "old" || saved.ImagePath != "original.jpg" || saved.VideoPath != "clip.mp4" {
		t.Fatal(saved, err)
	}
}
