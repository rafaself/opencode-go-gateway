package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestServerBehaviorPersistsPartialStreamEvents(t *testing.T) {
	outputDir := t.TempDir()
	server, err := Listen(Config{OutputDir: outputDir, FixturePrefix: "cancel", ResponseMode: ResponseText})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	writer := &serverBehaviorFailingResponseWriter{
		failAfter: 2,
		err:       errors.New("client disconnected"),
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.3-codex","stream":true}`))
	server.ServeHTTP(writer, request)

	if writer.status != http.StatusOK {
		t.Fatalf("status = %d, want %d", writer.status, http.StatusOK)
	}
	fixture := readServerBehaviorFixture(t, filepath.Join(outputDir, "cancel-001.json"))
	if want := []string{"response.created", "response.in_progress"}; !reflect.DeepEqual(fixture.Response.EventTypes, want) {
		t.Fatalf("event types = %v, want %v", fixture.Response.EventTypes, want)
	}
}

func TestServerBehaviorPreservesGenericStreamError(t *testing.T) {
	events, err := responseEvents(ResponseText, "gpt-5.3-codex", "capture acknowledged", 1)
	if err != nil {
		t.Fatal(err)
	}
	streamErr := errors.New("synthetic stream failure")
	writer := &serverBehaviorFailingResponseWriter{failAfter: 1, err: streamErr}

	written, err := writeResponseStreamWithContext(context.Background(), writer, events)
	if !errors.Is(err, streamErr) {
		t.Fatalf("stream error = %v, want %v", err, streamErr)
	}
	if want := []string{"response.created"}; !reflect.DeepEqual(written, want) {
		t.Fatalf("written event types = %v, want %v", written, want)
	}
}

func TestServerBehaviorAllocatesNextFixtureAfterRestart(t *testing.T) {
	outputDir := t.TempDir()

	server, err := Listen(Config{OutputDir: outputDir, FixturePrefix: "repeat", ResponseMode: ResponseText})
	if err != nil {
		t.Fatal(err)
	}
	serveServerBehaviorRequest(t, server)
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(outputDir, "repeat-001.json")
	firstFixture, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}

	server, err = Listen(Config{OutputDir: outputDir, FixturePrefix: "repeat", ResponseMode: ResponseText})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serveServerBehaviorRequest(t, server)

	if got, err := os.ReadFile(firstPath); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, firstFixture) {
		t.Fatal("the first fixture was overwritten after restart")
	}
	second := readServerBehaviorFixture(t, filepath.Join(outputDir, "repeat-002.json"))
	if second.CaptureNumber != 2 {
		t.Fatalf("second capture number = %d, want 2", second.CaptureNumber)
	}
}

func TestServerBehaviorAllocatesDistinctFixturesAcrossServers(t *testing.T) {
	outputDir := t.TempDir()
	servers := make([]*Server, 2)
	for index := range servers {
		server, err := Listen(Config{OutputDir: outputDir, FixturePrefix: "parallel", ResponseMode: ResponseText})
		if err != nil {
			t.Fatal(err)
		}
		servers[index] = server
		defer server.Close()
	}

	status := make(chan int, len(servers))
	for _, server := range servers {
		go func(server *Server) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.3-codex","stream":true}`))
			server.ServeHTTP(recorder, request)
			status <- recorder.Code
		}(server)
	}
	for range servers {
		if got := <-status; got != http.StatusOK {
			t.Fatalf("status = %d, want %d", got, http.StatusOK)
		}
	}

	for _, name := range []string{"parallel-001.json", "parallel-002.json"} {
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Fatalf("missing fixture %s: %v", name, err)
		}
	}
}

type serverBehaviorFailingResponseWriter struct {
	header           http.Header
	status           int
	successfulWrites int
	failAfter        int
	err              error
	body             bytes.Buffer
}

func (w *serverBehaviorFailingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *serverBehaviorFailingResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *serverBehaviorFailingResponseWriter) Write(payload []byte) (int, error) {
	if w.successfulWrites >= w.failAfter {
		return 0, w.err
	}
	w.successfulWrites++
	return w.body.Write(payload)
}

func (w *serverBehaviorFailingResponseWriter) Flush() {}

func readServerBehaviorFixture(t *testing.T, path string) Fixture {
	t.Helper()
	fixtureBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture Fixture
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func serveServerBehaviorRequest(t *testing.T, server *Server) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.3-codex","stream":true}`))
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
