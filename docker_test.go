package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// resolveDockerHost — unit tests (no real Docker daemon needed)
// ---------------------------------------------------------------------------

func TestResolveDockerHost_Default(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")

	got, err := resolveDockerHost()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.baseURL != "http://docker" {
		t.Errorf("baseURL = %q, want %q", got.baseURL, "http://docker")
	}
	if got.client == nil {
		t.Error("client is nil")
	}
}

func TestResolveDockerHost_ExplicitUnix(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/alt.sock")

	got, err := resolveDockerHost()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The baseURL is always "http://docker" for Unix transports; the actual
	// socket path is encoded in the transport's DialContext closure.
	if got.baseURL != "http://docker" {
		t.Errorf("baseURL = %q, want %q", got.baseURL, "http://docker")
	}
}

func TestResolveDockerHost_TCP(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://socket-proxy:2375")

	got, err := resolveDockerHost()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.baseURL != "http://socket-proxy:2375" {
		t.Errorf("baseURL = %q, want %q", got.baseURL, "http://socket-proxy:2375")
	}
}

func TestResolveDockerHost_HTTP(t *testing.T) {
	t.Setenv("DOCKER_HOST", "http://socket-proxy:2375")

	got, err := resolveDockerHost()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.baseURL != "http://socket-proxy:2375" {
		t.Errorf("baseURL = %q, want %q", got.baseURL, "http://socket-proxy:2375")
	}
}

func TestResolveDockerHost_NoHostComponent(t *testing.T) {
	// A URL that parses successfully but has no host — should be rejected.
	t.Setenv("DOCKER_HOST", "tcp://")

	_, err := resolveDockerHost()
	if err == nil {
		t.Fatal("expected an error for DOCKER_HOST with no host, got nil")
	}
	if !strings.Contains(err.Error(), "no host component") {
		t.Errorf("error %q should mention 'no host component'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// isDockerAvailable — TCP path (httptest server, no real daemon)
// ---------------------------------------------------------------------------

// withDockerClient temporarily installs a *dockerTarget and restores the
// original after the test.
func withDockerClient(t *testing.T, dt *dockerTarget, fn func()) {
	t.Helper()
	orig := dockerClient
	dockerClient = dt
	defer func() { dockerClient = orig }()
	fn()
}

func TestIsDockerAvailable_TCPReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dt := &dockerTarget{
		baseURL: srv.URL,
		client:  srv.Client(),
	}

	withDockerClient(t, dt, func() {
		if !isDockerAvailable() {
			t.Error("isDockerAvailable() = false, want true")
		}
	})
}

func TestIsDockerAvailable_TCPUnreachable(t *testing.T) {
	dt := &dockerTarget{
		baseURL: "http://127.0.0.1:19999",
		client:  &http.Client{},
	}

	withDockerClient(t, dt, func() {
		if isDockerAvailable() {
			t.Error("isDockerAvailable() = true, want false for unreachable host")
		}
	})
}

func TestIsDockerAvailable_NilClient(t *testing.T) {
	withDockerClient(t, nil, func() {
		if isDockerAvailable() {
			t.Error("isDockerAvailable() = true with nil dockerClient, want false")
		}
	})
}

// ---------------------------------------------------------------------------
// dockerURL — unit tests
// ---------------------------------------------------------------------------

func TestDockerURL_NilClient(t *testing.T) {
	withDockerClient(t, nil, func() {
		got := dockerURL("/containers/json")
		if got != "http://docker/containers/json" {
			t.Errorf("dockerURL = %q, want %q", got, "http://docker/containers/json")
		}
	})
}

func TestDockerURL_TCPClient(t *testing.T) {
	dt := &dockerTarget{baseURL: "http://proxy:2375", client: &http.Client{}}
	withDockerClient(t, dt, func() {
		got := dockerURL("/containers/json?all=1")
		if got != "http://proxy:2375/containers/json?all=1" {
			t.Errorf("dockerURL = %q, want %q", got, "http://proxy:2375/containers/json?all=1")
		}
	})
}

// ---------------------------------------------------------------------------
// findDockerContainer — over a fake TCP server (no Unix socket required)
// ---------------------------------------------------------------------------

func TestFindDockerContainer_TCP(t *testing.T) {
	containers := []DockerContainerSummary{
		{
			ID:     "abc1234567890000",
			Names:  []string{"/myapp"},
			Image:  "myapp:latest",
			State:  "running",
			Status: "Up 2 hours",
			Labels: map[string]string{},
		},
	}
	payload, _ := json.Marshal(containers)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
	defer srv.Close()

	dt := &dockerTarget{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	withDockerClient(t, dt, func() {
		got, err := findDockerContainer(dt.client, "myapp")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("container = nil, want match")
		}
		if got.Image != "myapp:latest" {
			t.Errorf("Image = %q, want %q", got.Image, "myapp:latest")
		}
	})
}

func TestFindDockerContainer_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	dt := &dockerTarget{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	withDockerClient(t, dt, func() {
		got, err := findDockerContainer(dt.client, "nonexistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil container, got %+v", got)
		}
	})
}

func TestFindDockerContainer_NilClient(t *testing.T) {
	_, err := findDockerContainer(nil, "any")
	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}
