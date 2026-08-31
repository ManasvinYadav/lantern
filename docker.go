package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// ---------------------------------------------------------------------------
// Discovery registry
//
// Records which service names the most recent discovery pass saw, so a
// service's source can be reported without a Docker socket round-trip.
// buildServiceSummary runs on every WebSocket broadcast, and handleGetServices
// runs per service on every poll, so this lookup has to stay O(1) and cheap to
// lock — calling findDockerContainer from either would be a real regression.
// ---------------------------------------------------------------------------

var (
	dockerDiscoveredMu sync.RWMutex
	dockerDiscovered   = map[string]struct{}{}
)

// setDockerDiscovered replaces the registry wholesale at the end of a
// successful pass, so a container that disappears between polls stops being
// reported as docker-sourced. A failed pass returns before calling this, which
// leaves the previous snapshot in place rather than blanking every service.
func setDockerDiscovered(names map[string]struct{}) {
	dockerDiscoveredMu.Lock()
	dockerDiscovered = names
	dockerDiscoveredMu.Unlock()
}

func isDockerDiscovered(name string) bool {
	dockerDiscoveredMu.RLock()
	defer dockerDiscoveredMu.RUnlock()
	_, ok := dockerDiscovered[name]
	return ok
}

// serviceSource classifies where a service's status comes from:
//
//	"monitor" — Lantern actively probes it (it has an enabled active_monitors row)
//	"docker"  — Docker discovery saw it on the most recent pass
//	"host"    — anything else, i.e. something pushing to POST /api/status
//
// Monitor wins over docker: if an operator has configured an explicit probe for
// a container, that probe is the authoritative source of its status.
func serviceSource(serviceName, monitorType string) string {
	if monitorType != "" {
		return "monitor"
	}
	if isDockerDiscovered(serviceName) {
		return "docker"
	}
	return "host"
}

const dockerSocketPath = "/var/run/docker.sock"

// maxLogFrameSize caps how much of a single Docker log-stream frame we'll
// allocate up front. frameLen comes straight off the wire as a uint32 (up
// to ~4GB); a stream-framing desync (or a misbehaving daemon) could claim
// an implausibly large frame, and make([]byte, frameLen) would try to
// allocate that much in one shot. The socket is local/trusted-only, so this
// is defense-in-depth rather than a real remote attack surface.
const maxLogFrameSize = 10 * 1024 * 1024 // 10MB

// ---------------------------------------------------------------------------
// Docker connection layer
//
// Lantern talks to the Docker daemon through a single *dockerTarget that is
// resolved once at startup. Depending on the DOCKER_HOST environment variable
// the transport is either:
//
//   - Unix socket (default, or explicit unix:///path): the classic mount at
//     /var/run/docker.sock. The HTTP client wraps the socket in a fake
//     "http://docker" host so the standard net/http URL parser is happy.
//
//   - TCP / HTTP (tcp:// or http://): plain HTTP to a remote daemon or a
//     socket proxy such as docker-socket-proxy.
//
//   - TLS / HTTPS (https:// or DOCKER_TLS_VERIFY=1): mutual-TLS connection
//     using certificates from DOCKER_CERT_PATH (defaults to ~/.docker).
//
// All Docker API request URLs are built through dockerURL(), which prepends
// dockerClient.baseURL, so the rest of the file never references localhost.
// ---------------------------------------------------------------------------

// dockerTarget holds everything needed to make Docker API calls.
type dockerTarget struct {
	// baseURL is the scheme+host prefix for every Docker API request,
	// e.g. "http://socket-proxy:2375" or "http://docker" (Unix socket).
	baseURL string
	client  *http.Client
}

// dockerClient is initialised once by initDockerClient() and then read-only.
var dockerClient *dockerTarget

// resolveDockerHost parses DOCKER_HOST (falling back to the Unix socket)
// and returns a ready *dockerTarget. It is a pure function with no side
// effects, which makes it directly testable.
func resolveDockerHost() (*dockerTarget, error) {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))

	// --- Unix socket (default or explicit) ---
	if host == "" || strings.HasPrefix(host, "unix://") {
		socketPath := dockerSocketPath
		if strings.HasPrefix(host, "unix://") {
			socketPath = strings.TrimPrefix(host, "unix://")
		}
		return &dockerTarget{
			baseURL: "http://docker",
			client: &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, "unix", socketPath)
					},
					DisableKeepAlives: true,
				},
				Timeout: 30 * time.Second,
			},
		}, nil
	}

	// --- TCP / HTTP / HTTPS ---
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST %q is not a valid URL: %w", host, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("DOCKER_HOST %q has no host component", host)
	}

	tlsVerify := os.Getenv("DOCKER_TLS_VERIFY") == "1"
	certPath := os.Getenv("DOCKER_CERT_PATH")

	var transport http.RoundTripper
	if tlsVerify || strings.EqualFold(u.Scheme, "https") {
		tlsCfg, err := buildDockerTLSConfig(certPath)
		if err != nil {
			return nil, fmt.Errorf("DOCKER_HOST TLS setup: %w", err)
		}
		transport = &http.Transport{TLSClientConfig: tlsCfg, DisableKeepAlives: true}
		u.Scheme = "https"
	} else {
		transport = &http.Transport{DisableKeepAlives: true}
		u.Scheme = "http"
	}

	baseURL := u.Scheme + "://" + u.Host
	return &dockerTarget{
		baseURL: baseURL,
		client:  &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}, nil
}

// buildDockerTLSConfig loads ca.pem, cert.pem, and key.pem from certPath
// (or ~/.docker when certPath is empty) and returns a *tls.Config suitable
// for mutual TLS with the Docker daemon.
func buildDockerTLSConfig(certPath string) (*tls.Config, error) {
	if certPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory for DOCKER_CERT_PATH: %w", err)
		}
		certPath = filepath.Join(home, ".docker")
	}

	caPEM, err := os.ReadFile(filepath.Join(certPath, "ca.pem"))
	if err != nil {
		return nil, fmt.Errorf("reading ca.pem from %s: %w", certPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid CA certificates found in %s/ca.pem", certPath)
	}

	cert, err := tls.LoadX509KeyPair(
		filepath.Join(certPath, "cert.pem"),
		filepath.Join(certPath, "key.pem"),
	)
	if err != nil {
		return nil, fmt.Errorf("loading cert/key from %s: %w", certPath, err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// initDockerClient resolves DOCKER_HOST once and stores the result in the
// package-level dockerClient. Called from main() before any Docker API use.
func initDockerClient() {
	t, err := resolveDockerHost()
	if err != nil {
		log.Printf("docker: cannot resolve DOCKER_HOST: %v — Docker features disabled", err)
		return
	}
	dockerClient = t

	// Surface the effective transport so operators know what was resolved.
	if t.baseURL == "http://docker" {
		log.Printf("docker: transport = Unix socket (%s)", dockerSocketPath)
	} else {
		log.Printf("docker: transport = TCP (%s)", t.baseURL)
	}
}

// isDockerAvailable reports whether the configured Docker endpoint is
// reachable. For Unix sockets this checks the socket file and dials it;
// for TCP endpoints it makes a lightweight GET /info request. Returns false
// when dockerClient is nil (i.e. initDockerClient() encountered an error).
func isDockerAvailable() bool {
	if dockerClient == nil {
		return false
	}

	// For Unix-socket clients, confirm the socket file exists and is
	// dialable before spending a full HTTP round-trip.
	if dockerClient.baseURL == "http://docker" {
		info, err := os.Stat(dockerSocketPath)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			return false
		}
		conn, err := net.DialTimeout("unix", dockerSocketPath, 500*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}

	// TCP: probe the daemon with a short-timeout GET /info.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerClient.baseURL+"/info", nil)
	if err != nil {
		return false
	}
	resp, err := dockerClient.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// dockerURL returns the full URL for a Docker API path, e.g.
// dockerURL("/containers/json?all=1") → "http://socket-proxy:2375/containers/json?all=1".
func dockerURL(path string) string {
	if dockerClient == nil {
		return "http://docker" + path
	}
	return dockerClient.baseURL + path
}

// dockerHTTPClient returns the shared HTTP client for Docker API calls.
// Callers must not mutate the returned client.
func dockerHTTPClient() *http.Client {
	if dockerClient == nil {
		return nil
	}
	return dockerClient.client
}

// DockerContainerSummary matches the item returned by GET /containers/json.
type DockerContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Ports   []struct {
		IP          string `json:"IP"`
		PrivatePort uint16 `json:"PrivatePort"`
		PublicPort  uint16 `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// DockerInspectResponse matches subset of GET /containers/{id}/json.
type DockerInspectResponse struct {
	ID      string   `json:"Id"`
	Created string   `json:"Created"`
	Name    string   `json:"Name"`
	Path    string   `json:"Path"`
	Args    []string `json:"Args"`
	State   struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		Dead       bool   `json:"Dead"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status        string `json:"Status"`
			FailingStreak int    `json:"FailingStreak"`
		} `json:"Health"`
	} `json:"State"`
	Image        string `json:"Image"`
	RestartCount int    `json:"RestartCount"`
	Platform     string `json:"Platform"`
	HostConfig   struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	Config struct {
		Image      string            `json:"Image"`
		Env        []string          `json:"Env"`
		WorkingDir string            `json:"WorkingDir"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Ports     map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
			Gateway   string `json:"Gateway"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Driver      string `json:"Driver"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// findDockerContainer searches active and stopped containers for one matching the service name.
func findDockerContainer(client *http.Client, serviceName string) (*DockerContainerSummary, error) {
	if client == nil {
		return nil, fmt.Errorf("docker client not available")
	}

	resp, err := client.Get(dockerURL("/containers/json?all=1"))
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("docker api error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var containers []DockerContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("failed to decode containers list: %w", err)
	}

	target := strings.ToLower(strings.TrimSpace(serviceName))
	// 1. Exact match against container name (stripped of leading slash)
	for _, c := range containers {
		for _, name := range c.Names {
			trimmed := strings.ToLower(strings.TrimPrefix(name, "/"))
			if trimmed == target {
				return &c, nil
			}
		}
	}

	// 2. Compose service label match (com.docker.compose.service == target)
	for _, c := range containers {
		if sName, ok := c.Labels["com.docker.compose.service"]; ok {
			if strings.ToLower(sName) == target {
				return &c, nil
			}
		}
	}

	// 3. Prefix/Suffix match (e.g. "lantern_app", "media-plex")
	for _, c := range containers {
		for _, name := range c.Names {
			trimmed := strings.ToLower(strings.TrimPrefix(name, "/"))
			if strings.HasSuffix(trimmed, "_"+target) || strings.HasSuffix(trimmed, "-"+target) || strings.HasPrefix(trimmed, target+"_") || strings.HasPrefix(trimmed, target+"-") {
				return &c, nil
			}
		}
	}

	return nil, nil
}

// parseDockerMuxLogs strips the 8-byte multiplex header if present from Docker logs.
func parseDockerMuxLogs(r io.Reader) string {
	var buf strings.Builder
	header := make([]byte, 8)

	for {
		_, err := io.ReadFull(r, header)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				if buf.Len() == 0 {
					all, _ := io.ReadAll(r)
					return string(append(header, all...))
				}
			}
			break
		}

		streamType := header[0] // 1: stdout, 2: stderr
		if streamType != 1 && streamType != 2 && streamType != 0 {
			remaining, _ := io.ReadAll(r)
			return string(header) + string(remaining)
		}

		frameLen := binary.BigEndian.Uint32(header[4:8])
		if frameLen == 0 {
			continue
		}

		if frameLen > maxLogFrameSize {
			// Implausibly large for a real log frame — almost certainly a
			// stream desync. Copy the first maxLogFrameSize bytes through
			// (so we don't just drop legitimate-but-huge content) and
			// discard the declared remainder so the next 8 bytes we read
			// are the next real frame header, not leftover frame data.
			if _, err := io.CopyN(&buf, r, int64(maxLogFrameSize)); err != nil {
				break
			}
			if _, err := io.CopyN(io.Discard, r, int64(frameLen)-int64(maxLogFrameSize)); err != nil {
				break
			}
			continue
		}

		frame := make([]byte, frameLen)
		_, err = io.ReadFull(r, frame)
		if err != nil {
			buf.Write(frame)
			break
		}
		buf.Write(frame)
	}

	return buf.String()
}

// ---------------------------------------------------------------------------
// Docker Management API Handlers
// ---------------------------------------------------------------------------

// handleGetDockerStatus handles GET /api/services/{name}/docker/status.
func handleGetDockerStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := strings.TrimSpace(vars["name"])

		if !isDockerAvailable() {
			writeJSON(w, http.StatusOK, map[string]any{
				"available": false,
				"message":   "Docker endpoint is not accessible",
			})
			return
		}

		client := dockerHTTPClient()
		container, err := findDockerContainer(client, name)
		if err != nil {
			log.Printf("handleGetDockerStatus error: %v", err)
			writeJSON(w, http.StatusOK, map[string]any{
				"available": true,
				"detected":  false,
				"error":     err.Error(),
			})
			return
		}

		if container == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"available": true,
				"detected":  false,
				"message":   "No matching Docker container found for service",
			})
			return
		}

		cleanName := ""
		if len(container.Names) > 0 {
			cleanName = strings.TrimPrefix(container.Names[0], "/")
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"available":      true,
			"detected":       true,
			"container_id":   container.ID[:12],
			"container_name": cleanName,
			"image":          container.Image,
			"state":          container.State,
			"status":         container.Status,
			"created":        container.Created,
		})
	}
}

// handlePostDockerRestart handles POST /api/services/{name}/docker/restart.
func handlePostDockerRestart(db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := strings.TrimSpace(vars["name"])

		scopedSvc := r.Context().Value(scopedServiceKey)
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		if !isDockerAvailable() {
			writeError(w, http.StatusServiceUnavailable, "Docker endpoint is not accessible")
			return
		}

		client := dockerHTTPClient()
		container, err := findDockerContainer(client, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed searching for container: %v", err))
			return
		}
		if container == nil {
			writeError(w, http.StatusNotFound, "No matching Docker container found for service")
			return
		}

		restartURL := dockerURL(fmt.Sprintf("/containers/%s/restart?t=10", container.ID))
		resp, err := client.Post(restartURL, "application/json", nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("Docker restart call failed: %v", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			writeError(w, resp.StatusCode, fmt.Sprintf("Docker returned error %d: %s", resp.StatusCode, string(b)))
			return
		}

		// Recorded through ingestStatusEvent rather than a direct INSERT, so the
		// metrics cache is invalidated and the change is broadcast like any
		// other. The status is "degraded", not "up": the container has only just
		// been asked to restart and is by definition not serving yet. Claiming
		// "up" painted a green card for something still coming back, and the
		// next real check would immediately contradict it.
		if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, name, "degraded",
			"Container restart initiated via Lantern Admin", time.Now().UTC(), 0); err != nil {
			log.Printf("handlePostDockerRestart: failed to record restart for %s: %v", name, err)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"message":      fmt.Sprintf("Container %s (%s) restart initiated", name, container.ID[:12]),
			"container_id": container.ID[:12],
		})
	}
}

// handleGetDockerLogs handles GET /api/services/{name}/docker/logs.
func handleGetDockerLogs() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := strings.TrimSpace(vars["name"])

		scopedSvc := r.Context().Value(scopedServiceKey)
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		if !isDockerAvailable() {
			writeError(w, http.StatusServiceUnavailable, "Docker endpoint is not accessible")
			return
		}

		tail := 100
		if t := r.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 1000 {
				tail = n
			}
		}

		client := dockerHTTPClient()
		container, err := findDockerContainer(client, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed searching container: %v", err))
			return
		}
		if container == nil {
			writeError(w, http.StatusNotFound, "No matching Docker container found for service")
			return
		}

		logsURL := dockerURL(fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&tail=%d&timestamps=1", container.ID, tail))
		resp, err := client.Get(logsURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed reading docker logs: %v", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			writeError(w, resp.StatusCode, string(b))
			return
		}

		parsedLogs := parseDockerMuxLogs(resp.Body)

		cleanName := ""
		if len(container.Names) > 0 {
			cleanName = strings.TrimPrefix(container.Names[0], "/")
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"service_name":   name,
			"container_id":   container.ID[:12],
			"container_name": cleanName,
			"tail":           tail,
			"logs":           parsedLogs,
		})
	}
}

// PortMapping is a clean representation of a published port.
type PortMapping struct {
	ContainerPort uint16 `json:"container_port"`
	HostPort      string `json:"host_port,omitempty"`
	HostIP        string `json:"host_ip,omitempty"`
	Type          string `json:"type"`
}

// MountMapping is a clean representation of a volume mount.
type MountMapping struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        string `json:"mode"`
	RW          bool   `json:"rw"`
}

// ServiceMetadataResponse combines Docker inspect details and Lantern internal telemetry.
type ServiceMetadataResponse struct {
	ServiceName    string `json:"service_name"`
	GroupName      string `json:"group_name"`
	Type           string `json:"type"` // "docker" | "host"
	DockerDetected bool   `json:"docker_detected"`

	// Docker specific details
	ContainerID   string         `json:"container_id,omitempty"`
	ContainerName string         `json:"container_name,omitempty"`
	Image         string         `json:"image,omitempty"`
	State         string         `json:"state,omitempty"`
	HealthStatus  string         `json:"health_status,omitempty"`
	CreatedAt     string         `json:"created_at,omitempty"`
	StartedAt     string         `json:"started_at,omitempty"`
	RestartCount  int            `json:"restart_count,omitempty"`
	IPAddress     string         `json:"ip_address,omitempty"`
	NetworkName   string         `json:"network_name,omitempty"`
	Ports         []PortMapping  `json:"ports,omitempty"`
	Mounts        []MountMapping `json:"mounts,omitempty"`

	// Telemetry & DB info
	TotalEventsRecorded int    `json:"total_events_recorded"`
	LastSeen            string `json:"last_seen"`
	MaintenanceEnabled  bool   `json:"maintenance_enabled"`
}

// handleGetServiceMetadata handles GET /api/services/{name}/metadata.
func handleGetServiceMetadata(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := strings.TrimSpace(vars["name"])
		if name == "" {
			writeError(w, http.StatusBadRequest, "service name is required")
			return
		}

		meta := ServiceMetadataResponse{
			ServiceName: name,
			Type:        "host",
		}

		// Query group name
		_ = db.QueryRow("SELECT group_name FROM service_groups WHERE service_name = ?", name).Scan(&meta.GroupName)

		// Query total events recorded
		_ = db.QueryRow("SELECT COUNT(*) FROM status_events WHERE service_name = ?", name).Scan(&meta.TotalEventsRecorded)

		// Query last event timestamp
		_ = db.QueryRow("SELECT timestamp FROM status_events WHERE service_name = ? ORDER BY id DESC LIMIT 1", name).Scan(&meta.LastSeen)

		// Query maintenance
		var maint int
		_ = db.QueryRow("SELECT enabled FROM service_maintenance WHERE service_name = ?", name).Scan(&maint)
		meta.MaintenanceEnabled = (maint == 1)

		// Check Docker info if available
		if isDockerAvailable() {
			client := dockerHTTPClient()
			container, err := findDockerContainer(client, name)
			if err == nil && container != nil {
				meta.DockerDetected = true
				meta.Type = "docker"
				meta.ContainerID = container.ID[:12]
				if len(container.Names) > 0 {
					meta.ContainerName = strings.TrimPrefix(container.Names[0], "/")
				}
				meta.Image = container.Image
				meta.State = container.State

				// Perform full inspect
				inspResp, err := client.Get(dockerURL(fmt.Sprintf("/containers/%s/json", container.ID)))
				if err == nil && inspResp.StatusCode == http.StatusOK {
					defer inspResp.Body.Close()
					var insp DockerInspectResponse
					if err := json.NewDecoder(inspResp.Body).Decode(&insp); err == nil {
						meta.CreatedAt = insp.Created
						meta.StartedAt = insp.State.StartedAt
						meta.RestartCount = insp.RestartCount
						if insp.State.Health != nil {
							meta.HealthStatus = insp.State.Health.Status
						}
						meta.IPAddress = insp.NetworkSettings.IPAddress
						for netName, netInfo := range insp.NetworkSettings.Networks {
							if meta.IPAddress == "" {
								meta.IPAddress = netInfo.IPAddress
							}
							meta.NetworkName = netName
							break
						}

						// Parse Ports
						ports := []PortMapping{}
						for pKey, bindings := range insp.NetworkSettings.Ports {
							parts := strings.Split(pKey, "/")
							var cPort uint16
							pType := "tcp"
							if len(parts) > 0 {
								if n, err := strconv.ParseUint(parts[0], 10, 16); err == nil {
									cPort = uint16(n)
								}
							}
							if len(parts) > 1 {
								pType = parts[1]
							}

							if len(bindings) > 0 {
								for _, b := range bindings {
									ports = append(ports, PortMapping{
										ContainerPort: cPort,
										HostPort:      b.HostPort,
										HostIP:        b.HostIP,
										Type:          pType,
									})
								}
							} else {
								ports = append(ports, PortMapping{
									ContainerPort: cPort,
									Type:          pType,
								})
							}
						}
						meta.Ports = ports

						// Parse Mounts
						mounts := []MountMapping{}
						for _, m := range insp.Mounts {
							mounts = append(mounts, MountMapping{
								Type:        m.Type,
								Source:      m.Source,
								Destination: m.Destination,
								Mode:        m.Mode,
								RW:          m.RW,
							})
						}
						meta.Mounts = mounts
					}
				}
			}
		}

		writeJSON(w, http.StatusOK, meta)
	}
}

// ---------------------------------------------------------------------------
// Native container discovery
// ---------------------------------------------------------------------------

// dockerHealthFromStatus extracts the healthcheck verdict Docker embeds in the
// human-readable Status line ("Up 4 days (healthy)"). Returns "" when the
// container declares no healthcheck, which is not the same as being unhealthy.
func dockerHealthFromStatus(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "(healthy)"):
		return "healthy"
	case strings.Contains(s, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(s, "health: starting"):
		return "starting"
	}
	return ""
}

// dockerStatusFor maps a container's Docker state and status line onto a
// Lantern status and a human message.
//
// A running container whose healthcheck has not yet passed reports "degraded"
// rather than "up": during warm-up the container is not yet serving, and
// claiming otherwise would paint a green card for something still booting.
// A container with no healthcheck at all is taken at its word and reports up.
//
// The Docker status line is already human-readable ("Exited (0) 4 hours ago"),
// so it is carried through verbatim as the message.
func dockerStatusFor(state, status string) (string, string) {
	msg := strings.TrimSpace(status)
	if msg == "" {
		msg = "state: " + state
	}

	switch state {
	case "running":
		switch dockerHealthFromStatus(status) {
		case "unhealthy", "starting":
			return "degraded", msg
		}
		return "up", msg
	case "restarting", "paused":
		return "degraded", msg
	case "exited", "dead", "created", "removing":
		return "down", msg
	default:
		return "unknown", msg
	}
}

// dockerServiceName picks the name Lantern records for a container: the first
// non-empty entry in Names, minus the leading slash Docker prefixes. Falls
// back to a short container ID so an unnamed container is still tracked.
func dockerServiceName(c DockerContainerSummary) string {
	for _, n := range c.Names {
		if t := strings.TrimPrefix(strings.TrimSpace(n), "/"); t != "" {
			return t
		}
	}
	if len(c.ID) >= 12 {
		return c.ID[:12]
	}
	return c.ID
}

// dockerDiscoveryIgnored reports whether a container has opted out of
// discovery via the lantern.ignore label.
func dockerDiscoveryIgnored(labels map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(labels["lantern.ignore"]), "true")
}

// runDockerDiscovery polls the local Docker daemon on an interval and records
// a heartbeat for every container it finds, so Lantern populates itself with
// no external push script. Containers are auto-registered simply by being
// ingested: the service list is derived from status_events.
//
// Intended to be started with `go` from main().
func runDockerDiscovery(db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub) {
	if !cfg.DockerDiscovery {
		log.Printf("docker discovery: disabled via LANTERN_DOCKER_DISCOVERY")
		return
	}
	if !isDockerAvailable() {
		if dockerClient != nil && dockerClient.baseURL != "http://docker" {
			log.Printf("docker discovery: %s unreachable, discovery inactive", dockerClient.baseURL)
		} else {
			log.Printf("docker discovery: %s unavailable, discovery inactive", dockerSocketPath)
		}
		return
	}

	interval := time.Duration(cfg.DockerPollSeconds) * time.Second
	client := dockerHTTPClient()
	log.Printf("docker discovery: active, polling every %s", interval)

	// One pass immediately, so a restart repopulates the dashboard without
	// waiting out a full interval first.
	dockerDiscoveryPass(client, db, cfg, dispatcher, hub)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		dockerDiscoveryPass(client, db, cfg, dispatcher, hub)
	}
}

// dockerDiscoveryPass performs a single discovery poll. Every container comes
// back in one /containers/json call, so this is one request per tick
// regardless of how many containers the host runs.
func dockerDiscoveryPass(client *http.Client, db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub) {
	start := time.Now()

	resp, err := client.Get(dockerURL("/containers/json?all=1"))
	if err != nil {
		log.Printf("docker discovery: list failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		log.Printf("docker discovery: docker api error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return
	}

	var containers []DockerContainerSummary
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		log.Printf("docker discovery: decode failed: %v", err)
		return
	}

	// One measurement for the whole batch. This is the cost of the daemon
	// query itself, shared by every container in the response — it is not a
	// per-container probe time, and should not be read as one.
	latencyMs := time.Since(start).Milliseconds()

	now := time.Now().UTC()
	recorded, skipped := 0, 0
	seen := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		if dockerDiscoveryIgnored(c.Labels) {
			skipped++
			continue
		}
		name := dockerServiceName(c)
		if name == "" {
			continue
		}
		// Registered before the write is attempted: a transient DB error
		// should not make a container look host-sourced for a whole interval.
		seen[name] = struct{}{}
		status, message := dockerStatusFor(c.State, c.Status)
		if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, name, status, message, now, latencyMs); err != nil {
			log.Printf("docker discovery: failed to record %s: %v", name, err)
			continue
		}
		recorded++
	}

	setDockerDiscovered(seen)

	log.Printf("docker discovery: recorded %d container(s), ignored %d, daemon query %dms", recorded, skipped, latencyMs)
}
