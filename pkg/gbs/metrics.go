package gbs

import "sync/atomic"

// GBMetrics 保存灰度发布需要的单调计数器，进程重启后归零。
type GBMetrics struct {
	registerRequests       atomic.Uint64
	registerSuccess        atomic.Uint64
	registerFailures       atomic.Uint64
	catalogRequests        atomic.Uint64
	catalogSuccess         atomic.Uint64
	catalogTimeouts        atomic.Uint64
	catalogPartial         atomic.Uint64
	mediaRequests          atomic.Uint64
	mediaSuccess           atomic.Uint64
	mediaFailures          atomic.Uint64
	mediaDisconnects       atomic.Uint64
	directStarted          atomic.Uint64
	directCompleted        atomic.Uint64
	directFailed           atomic.Uint64
	directCancelled        atomic.Uint64
	directBytes            atomic.Uint64
	annexGRequests         atomic.Uint64
	annexGAccepted         atomic.Uint64
	annexGRejected         atomic.Uint64
	annexGRateLimited      atomic.Uint64
	annexGBusinessFailures atomic.Uint64
}

type GBMetricsSnapshot struct {
	RegisterRequests       uint64 `json:"register_requests"`
	RegisterSuccess        uint64 `json:"register_success"`
	RegisterFailures       uint64 `json:"register_failures"`
	CatalogRequests        uint64 `json:"catalog_requests"`
	CatalogSuccess         uint64 `json:"catalog_success"`
	CatalogTimeouts        uint64 `json:"catalog_timeouts"`
	CatalogPartial         uint64 `json:"catalog_partial"`
	MediaRequests          uint64 `json:"media_requests"`
	MediaSuccess           uint64 `json:"media_success"`
	MediaFailures          uint64 `json:"media_failures"`
	MediaDisconnects       uint64 `json:"media_disconnects"`
	DirectStarted          uint64 `json:"direct_tcp_started"`
	DirectCompleted        uint64 `json:"direct_tcp_completed"`
	DirectFailed           uint64 `json:"direct_tcp_failed"`
	DirectCancelled        uint64 `json:"direct_tcp_cancelled"`
	DirectBytes            uint64 `json:"direct_tcp_bytes"`
	AnnexGRequests         uint64 `json:"annex_g_inbound_requests"`
	AnnexGAccepted         uint64 `json:"annex_g_inbound_accepted"`
	AnnexGRejected         uint64 `json:"annex_g_inbound_rejected"`
	AnnexGRateLimited      uint64 `json:"annex_g_inbound_rate_limited"`
	AnnexGBusinessFailures uint64 `json:"annex_g_business_failures"`
	AnnexGPending          uint64 `json:"annex_g_pending"`
}

func (m *GBMetrics) Snapshot() GBMetricsSnapshot {
	return GBMetricsSnapshot{
		RegisterRequests:       m.registerRequests.Load(),
		RegisterSuccess:        m.registerSuccess.Load(),
		RegisterFailures:       m.registerFailures.Load(),
		CatalogRequests:        m.catalogRequests.Load(),
		CatalogSuccess:         m.catalogSuccess.Load(),
		CatalogTimeouts:        m.catalogTimeouts.Load(),
		CatalogPartial:         m.catalogPartial.Load(),
		MediaRequests:          m.mediaRequests.Load(),
		MediaSuccess:           m.mediaSuccess.Load(),
		MediaFailures:          m.mediaFailures.Load(),
		MediaDisconnects:       m.mediaDisconnects.Load(),
		DirectStarted:          m.directStarted.Load(),
		DirectCompleted:        m.directCompleted.Load(),
		DirectFailed:           m.directFailed.Load(),
		DirectCancelled:        m.directCancelled.Load(),
		DirectBytes:            m.directBytes.Load(),
		AnnexGRequests:         m.annexGRequests.Load(),
		AnnexGAccepted:         m.annexGAccepted.Load(),
		AnnexGRejected:         m.annexGRejected.Load(),
		AnnexGRateLimited:      m.annexGRateLimited.Load(),
		AnnexGBusinessFailures: m.annexGBusinessFailures.Load(),
	}
}
