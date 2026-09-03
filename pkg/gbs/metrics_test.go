package gbs

import "testing"

func TestGBMetricsSnapshot(t *testing.T) {
	var metrics GBMetrics
	metrics.registerRequests.Add(2)
	metrics.registerSuccess.Add(1)
	metrics.registerFailures.Add(1)
	metrics.catalogRequests.Add(3)
	metrics.catalogSuccess.Add(2)
	metrics.catalogTimeouts.Add(1)
	metrics.catalogPartial.Add(1)
	metrics.mediaRequests.Add(4)
	metrics.mediaSuccess.Add(3)
	metrics.mediaFailures.Add(1)
	metrics.mediaDisconnects.Add(2)
	metrics.directStarted.Add(2)
	metrics.directCompleted.Add(1)
	metrics.directFailed.Add(1)
	metrics.directCancelled.Add(1)
	metrics.directBytes.Add(1024)
	metrics.annexGRequests.Add(5)
	metrics.annexGAccepted.Add(3)
	metrics.annexGRejected.Add(2)
	metrics.annexGRateLimited.Add(1)
	metrics.annexGBusinessFailures.Add(1)
	snapshot := metrics.Snapshot()
	if snapshot.RegisterRequests != 2 || snapshot.RegisterSuccess != 1 || snapshot.RegisterFailures != 1 ||
		snapshot.CatalogRequests != 3 || snapshot.CatalogSuccess != 2 || snapshot.CatalogTimeouts != 1 || snapshot.CatalogPartial != 1 ||
		snapshot.MediaRequests != 4 || snapshot.MediaSuccess != 3 || snapshot.MediaFailures != 1 || snapshot.MediaDisconnects != 2 ||
		snapshot.DirectStarted != 2 || snapshot.DirectCompleted != 1 || snapshot.DirectFailed != 1 || snapshot.DirectCancelled != 1 || snapshot.DirectBytes != 1024 ||
		snapshot.AnnexGRequests != 5 || snapshot.AnnexGAccepted != 3 || snapshot.AnnexGRejected != 2 || snapshot.AnnexGRateLimited != 1 || snapshot.AnnexGBusinessFailures != 1 {
		t.Fatalf("metrics snapshot = %+v", snapshot)
	}
}
