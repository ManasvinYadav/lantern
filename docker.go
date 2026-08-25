package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

const dockerSocketPath = "/var/run/docker.sock"

// maxLogFrameSize caps how much of a single Docker log-stream frame we'll
// allocate up front. frameLen comes straight off the wire as a uint32 (up
// to ~4GB); a stream-framing desync (or a misbehaving daemon) could claim
// an implausibly large frame, and make([]byte, frameLen) would try to
// allocate that much in one shot. The socket is local/trusted-only, so this
// is defense-in-depth rather than a real remote attack surface.
const maxLogFrameSize = 10 * 1024 * 1024 // 10MB

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

// isDockerSocketAvailable checks if /var/run/docker.sock exists and is dialable.
func isDockerSocketAvailable() bool {
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

// getDockerHTTPClient returns an HTTP client routed through the unix domain socket.
func getDockerHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", dockerSocketPath)
			},
			DisableKeepAlives: true,
		},
		Timeout: 30 * time.Second,
	}
}

// findDockerContainer searches active and stopped containers for one matching the service name.
func findDockerContainer(client *http.Client, serviceName string) (*DockerContainerSummary, error) {
	if client == nil {
		return nil, fmt.Errorf("docker client not available")
	}

	resp, err := client.Get("http://localhost/containers/json?all=1")
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

		if !isDockerSocketAvailable() {
			writeJSON(w, http.StatusOK, map[string]any{
				"available": false,
				"message":   "Docker socket is not accessible",
			})
			return
		}

		client := getDockerHTTPClient()
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
func handlePostDockerRestart(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		name := strings.TrimSpace(vars["name"])

		scopedSvc := r.Context().Value(scopedServiceKey)
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		if !isDockerSocketAvailable() {
			writeError(w, http.StatusServiceUnavailable, "Docker socket is not accessible")
			return
		}

		client := getDockerHTTPClient()
		container, err := findDockerContainer(client, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed searching for container: %v", err))
			return
		}
		if container == nil {
			writeError(w, http.StatusNotFound, "No matching Docker container found for service")
			return
		}

		restartURL := fmt.Sprintf("http://localhost/containers/%s/restart?t=10", container.ID)
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

		_, _ = db.Exec(
			`INSERT INTO status_events (service_name, status, message, timestamp) VALUES (?, 'up', 'Container restart initiated via Lantern Admin', ?)`,
			name, time.Now().UTC().Format(time.RFC3339),
		)

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

		if !isDockerSocketAvailable() {
			writeError(w, http.StatusServiceUnavailable, "Docker socket is not accessible")
			return
		}

		tail := 100
		if t := r.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 1000 {
				tail = n
			}
		}

		client := getDockerHTTPClient()
		container, err := findDockerContainer(client, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("failed searching container: %v", err))
			return
		}
		if container == nil {
			writeError(w, http.StatusNotFound, "No matching Docker container found for service")
			return
		}

		logsURL := fmt.Sprintf("http://localhost/containers/%s/logs?stdout=1&stderr=1&tail=%d&timestamps=1", container.ID, tail)
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
		if isDockerSocketAvailable() {
			client := getDockerHTTPClient()
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
				inspResp, err := client.Get(fmt.Sprintf("http://localhost/containers/%s/json", container.ID))
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
	if !isDockerSocketAvailable() {
		log.Printf("docker discovery: %s unavailable, discovery inactive", dockerSocketPath)
		return
	}

	interval := time.Duration(cfg.DockerPollSeconds) * time.Second
	client := getDockerHTTPClient()
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

	resp, err := client.Get("http://localhost/containers/json?all=1")
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
	for _, c := range containers {
		if dockerDiscoveryIgnored(c.Labels) {
			skipped++
			continue
		}
		name := dockerServiceName(c)
		if name == "" {
			continue
		}
		status, message := dockerStatusFor(c.State, c.Status)
		if _, err := ingestStatusEvent(db, cfg, dispatcher, hub, name, status, message, now, latencyMs); err != nil {
			log.Printf("docker discovery: failed to record %s: %v", name, err)
			continue
		}
		recorded++
	}

	log.Printf("docker discovery: recorded %d container(s), ignored %d, daemon query %dms", recorded, skipped, latencyMs)
}
