package gbs

import "testing"

func TestParseRTPDownloadFileSize(t *testing.T) {
	body := []byte("v=0\r\no=device 0 0 IN IP4 192.0.2.10\r\ns=Download\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 6000 RTP/AVP 96\r\na=rtpmap:96 PS/90000\r\na=filesize:12345\r\n")
	size, known, err := parseRTPDownloadFileSize(body)
	if err != nil || !known || size != 12345 {
		t.Fatalf("size=%d known=%v err=%v", size, known, err)
	}
	if _, _, err := parseRTPDownloadFileSize([]byte("v=0\r\no=device 0 0 IN IP4 192.0.2.10\r\ns=Download\r\nc=IN IP4 192.0.2.10\r\nt=0 0\r\nm=video 6000 RTP/AVP 96\r\na=filesize:-1\r\n")); err == nil {
		t.Fatal("negative filesize accepted")
	}
}
