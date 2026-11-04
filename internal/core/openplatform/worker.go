package openplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InstallOutbox 必须在事件提交前注册；与原事件写入共享事务，失败时一并回滚。
func (s *Service) InstallOutbox() error {
	return s.DB.Callback().Create().After("gorm:after_create").Before("gorm:commit_or_rollback_transaction").Register("owl:openplatform_outbox", func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "events" || tx.RowsAffected == 0 {
			return
		}
		var events []*event.Event
		switch v := tx.Statement.Dest.(type) {
		case *event.Event:
			events = []*event.Event{v}
		case *[]*event.Event:
			events = *v
		case *[]event.Event:
			for i := range *v {
				events = append(events, &(*v)[i])
			}
		default:
			tx.AddError(fmt.Errorf("不支持的开放事件写入类型"))
			return
		}
		for _, ev := range events {
			payload := marshal(map[string]any{"id": ev.ID, "did": ev.DID, "cid": ev.CID, "started_at": ev.StartedAt, "ended_at": ev.EndedAt, "label": ev.Label, "score": ev.Score, "model": ev.Model})
			box := Outbox{EventID: ev.ID, DeviceID: ev.DID, ChannelID: ev.CID, Label: ev.Label, Payload: payload}
			if e := tx.Session(&gorm.Session{NewDB: true}).Clauses(clause.OnConflict{DoNothing: true}).Create(&box).Error; e != nil {
				tx.AddError(e)
				return
			}
		}
	})
}
func (s *Service) eventVisibility(a App, sub Subscription, box Outbox) (bool, error) {
	if len(sub.Labels) > 0 && !Has(sub.Labels, box.Label) || len(sub.ChannelIDs) > 0 && !Has(sub.ChannelIDs, box.ChannelID) {
		return false, nil
	}
	var err error
	if box.ChannelID != "" {
		var ch ipc.Channel
		ch, err = s.Channel(a, box.ChannelID)
		if err == nil && box.DeviceID != "" && box.DeviceID != ch.DID {
			return false, nil
		}
	} else {
		err = s.CheckDevice(a, box.DeviceID)
	}
	if err != nil {
		if AsError(err).Status == 404 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
func (s *Service) visibleEvent(a App, sub Subscription, box Outbox) bool {
	visible, e := s.eventVisibility(a, sub, box)
	return e == nil && visible
}
func (s *Service) fanout() {
	var boxes []Outbox
	if s.DB.Where("done = ?", false).Order("id").Limit(100).Find(&boxes).Error != nil {
		return
	}
	for _, box := range boxes {
		var subs []Subscription
		if s.DB.Where("status = ? AND created_at <= ? AND (expires_at IS NULL OR expires_at > ?)", "active", box.CreatedAt, time.Now()).Find(&subs).Error != nil {
			return
		}
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			read := &Service{DB: tx}
			for _, sub := range subs {
				p, e := read.Current(sub.AppID, sub.CredentialID)
				if e != nil && AsError(e).Status == 503 {
					return e
				}
				visible, err := read.eventVisibility(p.App, sub, box)
				if err != nil {
					return err
				}
				if e != nil || s.Scope(p.App, "event:subscribe") != nil || !visible {
					continue
				}
				d := Delivery{ID: uuid.NewString(), AppID: sub.AppID, SubscriptionID: sub.ID, EventID: box.EventID, OutboxID: box.ID, State: "queued", NextRetryAt: time.Now()}
				if e = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&d).Error; e != nil {
					return e
				}
			}
			return tx.Model(&Outbox{}).Where("id = ?", box.ID).Update("done", true).Error
		})
		if err != nil {
			return
		}
	}
}
func (s *Service) deliver(ctx context.Context, d Delivery) {
	var sub Subscription
	var box Outbox
	var target CallbackTarget
	claimed := false
	cancel := func(reason string) {
		q := s.DB.Model(&Delivery{}).Where("id = ?", d.ID)
		if !claimed {
			q = q.Where("state = ? OR (state = ? AND lease_until < ?)", "queued", "sending", time.Now())
		} else {
			q = q.Where("state = ?", "sending")
		}
		_ = q.Updates(map[string]any{"state": "cancelled", "error": reason}).Error
	}
	load := func(out any, query string, args ...any) bool {
		err := s.DB.First(out, append([]any{query}, args...)...).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				cancel("订阅、事件或回调目标已不可用")
			}
			return false
		}
		return true
	}
	if !load(&sub, "id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", d.SubscriptionID, "active", time.Now()) {
		return
	}
	p, e := s.Current(sub.AppID, sub.CredentialID)
	if e != nil {
		if AsError(e).Status != 503 {
			cancel("应用或凭据不可用")
		}
		return
	}
	if s.Scope(p.App, "event:subscribe") != nil {
		cancel("应用未授权订阅")
		return
	}
	if !load(&box, "id = ?", d.OutboxID) {
		return
	}
	visible, err := s.eventVisibility(p.App, sub, box)
	if err != nil {
		return
	}
	if !visible {
		cancel("事件不在授权范围")
		return
	}
	if !load(&target, "id = ? AND app_id = ? AND status = ?", sub.CallbackTargetID, sub.AppID, "approved") {
		return
	}
	if len(sub.Keys) == 0 {
		cancel("回调签名密钥不可用")
		return
	}

	if !s.Limit("delivery:"+p.App.ID, p.App.DeliveryLimit, p.App.DeliveryLimit) {
		return
	}
	now := time.Now()
	r := s.DB.Model(&Delivery{}).Where("id = ? AND ((state = ? AND next_retry_at <= ?) OR (state = ? AND lease_until < ?))", d.ID, "queued", now, "sending", now).Updates(map[string]any{"state": "sending", "lease_until": now.Add(30 * time.Second), "attempt": gorm.Expr("attempt + 1")})
	if r.Error != nil || r.RowsAffected != 1 {
		return
	}
	claimed = true
	d.Attempt++
	key := sub.Keys[len(sub.Keys)-1]
	secret, e := s.Decrypt(sub.AppID, "callback:"+sub.ID+":"+key.ID, key.Secret)
	if e != nil {
		slog.Error("开放平台回调密钥不可用，保留投递待恢复", "subscription_id", sub.ID)
		return
	}
	var payload map[string]any
	if json.Unmarshal([]byte(box.Payload), &payload) != nil {
		cancel("事件快照不可用")
		return
	}
	payload["event_id"] = box.EventID
	payload["delivery_id"] = d.ID
	if Has(p.App.Scopes, "event:image") {
		raw, t, err := s.IssueTicket(p, "event", strconv.FormatInt(box.EventID, 10))
		if err == nil {
			payload["image_url"] = s.Config.PublicBaseURL + "/openmedia/v1/event/" + strconv.FormatInt(box.EventID, 10) + "/content?ticket=" + raw
			payload["image_expires_at"] = t.ExpiresAt
		}
	}
	body, _ := json.Marshal(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := Random(24)
	digest := Hash(body)
	req, e := http.NewRequestWithContext(ctx, "POST", target.URL, bytes.NewReader(body))
	if e != nil {
		cancel("回调请求无法构造")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Owl-Callback-Key-ID", key.ID)
	req.Header.Set("X-Owl-Timestamp", ts)
	req.Header.Set("X-Owl-Nonce", nonce)
	req.Header.Set("X-Owl-Event-ID", strconv.FormatInt(box.EventID, 10))
	req.Header.Set("X-Owl-Delivery-ID", d.ID)
	req.Header.Set("X-Owl-Content-SHA256", digest)
	req.Header.Set("Authorization", "OWL-CALLBACK-HMAC-SHA256 Signature="+CallbackSign(secret, CallbackCanonical(ts, nonce, strconv.FormatInt(box.EventID, 10), d.ID, digest)))
	// 发出前再读一次当前状态，防止排队期间撤权；已发出的内容无法追回。
	latest, check := s.Current(sub.AppID, sub.CredentialID)
	if check != nil && AsError(check).Status == 503 {
		return
	}
	if check != nil || latest.App.Revision != p.App.Revision {
		cancel("投递前授权已变化")
		return
	}
	var currentSub Subscription
	var currentTarget CallbackTarget
	if !load(&currentSub, "id = ? AND status = ?", sub.ID, "active") || !load(&currentTarget, "id = ? AND status = ?", target.ID, "approved") {
		return
	}
	visible, err = s.eventVisibility(latest.App, currentSub, box)
	if err != nil {
		return
	}
	if currentSub.Revision != sub.Revision || currentTarget.Revision != target.Revision || !visible {
		cancel("投递前订阅或目标已变化")
		return
	}
	targetURL, parseErr := url.Parse(target.URL)
	if parseErr != nil || !s.Config.AllowsScheme(targetURL.Scheme) {
		cancel("回调目标协议已被平台禁用")
		return
	}
	client := s.callbackClient(target)
	sendCtx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()
	doneWatch := make(chan struct{})
	defer close(doneWatch)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-doneWatch:
				return
			case <-sendCtx.Done():
				return
			case <-ticker.C:
				current, err := s.Current(sub.AppID, sub.CredentialID)
				var ss Subscription
				var tt CallbackTarget
				if err != nil || current.App.Revision != p.App.Revision || s.DB.First(&ss, "id = ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", sub.ID, "active", time.Now()).Error != nil || ss.Revision != sub.Revision || s.DB.First(&tt, "id = ? AND status = ?", target.ID, "approved").Error != nil || tt.Revision != target.Revision || !s.visibleEvent(current.App, ss, box) {
					cancelSend()
					return
				}
			}
		}
	}()
	response, sendErr := client.Do(req.WithContext(sendCtx))
	client.CloseIdleConnections()
	status := 0
	if response != nil {
		status = response.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}
	state := "queued"
	message := ""
	if sendErr == nil && status >= 200 && status < 300 {
		state = "delivered"
	} else if d.Attempt >= 8 {
		state = "dead"
		message = "超过最大投递次数"
	} else if status >= 300 && status < 500 && status != 408 && status != 429 {
		state = "failed"
		message = "接收方拒绝或返回重定向"
	} else {
		message = "网络异常或接收方暂不可用"
	}
	delay := time.Second * time.Duration(1<<min(d.Attempt, 8))
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	delay = time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	_ = s.DB.Model(&Delivery{}).Where("id = ? AND state = ?", d.ID, "sending").Updates(map[string]any{"state": state, "last_status": status, "error": message, "next_retry_at": time.Now().Add(delay)}).Error
	s.Record(Audit{AppID: sub.AppID, CredentialID: sub.CredentialID, Action: "delivery.result", ResourceID: d.ID, Result: state, Status: status})
}
func (s *Service) maintainCommands(ctx context.Context) {
	var rows []Command
	if s.DB.Where("state IN ?", []string{"pending", "confirmed", "unknown", "stop_failed"}).Limit(100).Find(&rows).Error != nil {
		return
	}
	for _, row := range rows {
		if row.State == "stop_failed" && row.NextStopAt.After(time.Now()) {
			continue
		}
		p, e := s.Current(row.AppID, row.CredentialID)
		if e == nil {
			_, e = s.Validate(p, "ptz:control", row.ChannelID)
		}
		if e != nil || p.App.Revision != row.Revision || !row.Deadline.After(time.Now()) || row.State == "pending" || row.State == "unknown" || row.State == "stop_failed" {
			lock := s.channelLock(row.ChannelID)
			if lock.TryLock() {
				s.Spawn(func() {
					defer lock.Unlock()
					if e := s.stopLocked(ctx, row); e != nil {
						slog.Warn("开放平台云台停止失败", "command_id", row.ID)
					}
				})
			}
		}
	}
}
func (s *Service) cleanup() {
	now := time.Now()
	// 复合主键表逐行删，其余表按主键分批，兼容三种数据库。
	var nonces []Nonce
	s.DB.Where("expires_at < ?", now).Limit(500).Find(&nonces)
	for _, v := range nonces {
		s.DB.Delete(&v)
	}
	var idem []Idempotency
	s.DB.Where("expires_at < ?", now).Limit(200).Find(&idem)
	for _, v := range idem {
		s.DB.Delete(&v)
	}
	var tickets []Ticket
	s.DB.Where("expires_at < ?", now.Add(-time.Hour)).Limit(500).Find(&tickets)
	for _, v := range tickets {
		s.DB.Delete(&v)
	}
	var logs []Audit
	s.DB.Where("created_at < ?", now.Add(-90*24*time.Hour)).Limit(200).Find(&logs)
	for _, v := range logs {
		s.DB.Delete(&v)
	}
	var expired []Snapshot
	if s.DB.Where("expires_at < ? AND path <> ''", now).Limit(100).Find(&expired).Error == nil {
		for _, row := range expired {
			_ = os.Remove(row.Path)
			s.DB.Model(&row).Updates(map[string]any{"path": "", "state": "expired"})
		}
	}
	s.DB.Model(&Session{}).Where("expires_at < ? AND state IN ?", now, []string{"ready", "preparing"}).Update("state", "expired")
	var deliveries []Delivery
	s.DB.Where("created_at < ? AND state IN ?", now.Add(-30*24*time.Hour), []string{"delivered", "dead", "failed", "cancelled"}).Limit(200).Find(&deliveries)
	for _, v := range deliveries {
		s.DB.Delete(&v)
	}
	var boxes []Outbox
	s.DB.Where("done = ? AND created_at < ?", true, now.Add(-30*24*time.Hour)).Limit(200).Find(&boxes)
	for _, v := range boxes {
		var n int64
		s.DB.Model(&Delivery{}).Where("outbox_id = ?", v.ID).Count(&n)
		if n == 0 {
			s.DB.Delete(&v)
		}
	}
}
func (s *Service) run(ctx context.Context) {
	jobs := make(chan Delivery, 8)
	var pool sync.WaitGroup
	for range 8 {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for d := range jobs {
				s.deliveryMu.Lock()
				if s.delivering == nil {
					s.delivering = map[string]int{}
				}
				var app App
				err := s.DB.First(&app, "id = ?", d.AppID).Error
				if err != nil || s.delivering[d.AppID] >= app.DeliveryLimit {
					s.deliveryMu.Unlock()
					continue
				}
				s.delivering[d.AppID]++
				s.deliveryMu.Unlock()
				s.deliver(ctx, d)
				s.deliveryMu.Lock()
				s.delivering[d.AppID]--
				s.deliveryMu.Unlock()
			}
		}()
	}
	var control sync.WaitGroup
	control.Add(1)
	go func() {
		defer control.Done()
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			s.maintainCommands(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	defer func() {
		close(jobs)
		pool.Wait()
		control.Wait()
		var rows []Command
		s.DB.Where("state IN ?", []string{"pending", "confirmed", "unknown", "stop_failed"}).Find(&rows)
		var shutdown sync.WaitGroup
		for _, row := range rows {
			shutdown.Add(1)
			go func() {
				defer shutdown.Done()
				lock := s.channelLock(row.ChannelID)
				lock.Lock()
				defer lock.Unlock()
				_ = s.stopLocked(context.Background(), row)
			}()
		}
		shutdown.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticks++
			s.fanout()
			var pending []Delivery
			if s.DB.Where("(state = ? AND next_retry_at <= ?) OR (state = ? AND lease_until < ?)", "queued", time.Now(), "sending", time.Now()).Order("next_retry_at").Limit(100).Find(&pending).Error == nil {
				for _, d := range pending {
					select {
					case jobs <- d:
					default:
					}
				}
			}
			if ticks%60 == 0 {
				s.cleanup()
			}
		}
	}
}
