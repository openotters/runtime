package neighbor_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openotters/runtime/pkg/neighbor"
)

func TestClient_SendMessageHappyPath(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)

		_ = json.NewEncoder(w).Encode(map[string]string{"response": "hi back"})
	}))
	t.Cleanup(srv.Close)

	c := neighbor.NewClient(neighbor.Config{Name: "alpha", URL: srv.URL, Token: "tok-123"})

	resp, err := c.SendMessage(context.Background(), "ping")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if resp != "hi back" {
		t.Errorf("resp = %q, want hi back", resp)
	}

	if gotPath != "/v1/chat" {
		t.Errorf("path = %q, want /v1/chat", gotPath)
	}

	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}

	if !strings.Contains(string(gotBody), `"prompt":"ping"`) {
		t.Errorf("body missing prompt: %s", string(gotBody))
	}

	if !strings.Contains(string(gotBody), `"session_id":"neighbor:alpha"`) {
		t.Errorf("body missing scoped session_id: %s", string(gotBody))
	}
}

func TestClient_SendMessageNon200Errors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	t.Cleanup(srv.Close)

	c := neighbor.NewClient(neighbor.Config{Name: "x", URL: srv.URL})

	_, err := c.SendMessage(context.Background(), "ping")
	if err == nil {
		t.Fatal("expected error on non-200, got nil")
	}

	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %v lacks status code", err)
	}
}

func TestClient_SendMessageMalformedJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)

	c := neighbor.NewClient(neighbor.Config{Name: "x", URL: srv.URL})

	_, err := c.SendMessage(context.Background(), "ping")
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}

	if !strings.Contains(err.Error(), "parsing") {
		t.Errorf("error %v doesn't mention parsing", err)
	}
}

func TestClient_SendMessageNoTokenSkipsAuthHeader(t *testing.T) {
	t.Parallel()

	var sawAuth bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"response": "ok"})
	}))
	t.Cleanup(srv.Close)

	c := neighbor.NewClient(neighbor.Config{Name: "x", URL: srv.URL})

	if _, err := c.SendMessage(context.Background(), "ping"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if sawAuth {
		t.Error("Authorization header sent even though Token was empty")
	}
}
