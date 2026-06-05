package scheduler

import "testing"

func TestNormalizeHTTPRequestLineSpacing(t *testing.T) {
	input := "GET /?View=Webboard&name=ShowThread&topicID=96*HTTP/1.1\r\nHost: example.com\r\n\r\n"
	want := "GET /?View=Webboard&name=ShowThread&topicID=96* HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if got := normalizeHTTPRequestLineSpacing(input); got != want {
		t.Fatalf("normalizeHTTPRequestLineSpacing() = %q, want %q", got, want)
	}

	alreadySpaced := "GET /?topicID=96* HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if got := normalizeHTTPRequestLineSpacing(alreadySpaced); got != alreadySpaced {
		t.Fatalf("normalizeHTTPRequestLineSpacing() changed spaced request: %q", got)
	}
}
