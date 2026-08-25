// Active monitoring (Phase 2): optional Uptime-Kuma-style checks that Lantern
// performs itself (HTTP, TCP, ICMP ping) on a schedule, as an alternative or
// supplement to services pushing their own status via POST /api/status.
// Results are written through the same ingestStatusEvent() path push updates
// use, so uptime %, incidents, and the history graph work identically
// regardless of which mechanism produced the event.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

var validMonitorTypes = map[string]bool{"http": true, "tcp": true, "ping": true}

const (
	minMonitorIntervalSeconds = 10
	maxMonitorIntervalSeconds = 3600
	monitorCheckTimeout       = 10 * time.Second
	monitorQueueSize          = 256
	certExpiryWarningDays     = 14
)

// ActiveMonitor is the persisted configuration for one service's active check.
type ActiveMonitor struct {
	ServiceName     string  `json:"service_name"`
	MonitorType     string  `json:"monitor_type"`
	Target          string  `json:"target"`
	IntervalSeconds int     `json:"interval_seconds"`
	Enabled         bool    `json:"enabled"`
	LastCheckedAt   *string `json:"last_checked_at"`
	// CertExpiryAt is set for "http" monitors whose target is HTTPS; nil
	// otherwise (plain HTTP, TCP, ping, or before the first check completes).
	CertExpiryAt      *string `json:"cert_expiry_at"`
	CertDaysRemaining *int    `json:"cert_days_remaining"`
	CertWarning       bool    `json:"cert_warning"`
}

// ---------------------------------------------------------------------------
// Worker pool: executes checks concurrently, never sequentially/blocking.
// ---------------------------------------------------------------------------

type monitorCheckJob struct {
	serviceName string
	monitorType string
	target      string
}

type monitorPool struct {
	jobs       chan monitorCheckJob
	httpClient *http.Client
	db         *sql.DB
	cfg        *Config
	dispatcher *webhookDispatcher
	hub        *wsHub
}

func newMonitorPool(db *sql.DB, cfg *Config, dispatcher *webhookDispatcher, hub *wsHub, workers int) *monitorPool {
	p := &monitorPool{
		jobs:       make(chan monitorCheckJob, monitorQueueSize),
		httpClient: &http.Client{Timeout: monitorCheckTimeout},
		db:         db,
		cfg:        cfg,
		dispatcher: dispatcher,
		hub:        hub,
	}
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *monitorPool) enqueue(job monitorCheckJob) {
	select {
	case p.jobs <- job:
	default:
		log.Printf("monitor check queue full, dropping check: service=%s type=%s", job.serviceName, job.monitorType)
	}
}

func (p *monitorPool) worker() {
	for job := range p.jobs {
		status, message, certExpiry := p.runCheck(job)

		if certExpiry != nil {
			daysLeft := int(time.Until(*certExpiry).Hours() / 24)
			if daysLeft <= certExpiryWarningDays {
				message = fmt.Sprintf("%s — ⚠️ TLS cert expires in %d day(s) (%s)", message, daysLeft, certExpiry.Format("2006-01-02"))
			}
		}

		ingestStatusEvent(p.db, p.cfg, p.dispatcher, p.hub, job.serviceName, status, message, time.Now().UTC())

		if certExpiry != nil {
			_, err := p.db.Exec(`UPDATE active_monitors SET last_checked_at = ?, cert_expiry_at = ? WHERE service_name = ?`,
				time.Now().UTC().Format(time.RFC3339), certExpiry.UTC().Format(time.RFC3339), job.serviceName)
			if err != nil {
				log.Printf("failed to update last_checked_at/cert_expiry_at for %s: %v", job.serviceName, err)
			}
		} else {
			_, err := p.db.Exec(`UPDATE active_monitors SET last_checked_at = ? WHERE service_name = ?`,
				time.Now().UTC().Format(time.RFC3339), job.serviceName)
			if err != nil {
				log.Printf("failed to update last_checked_at for %s: %v", job.serviceName, err)
			}
		}
	}
}

func (p *monitorPool) runCheck(job monitorCheckJob) (status, message string, certExpiry *time.Time) {
	switch job.monitorType {
	case "http":
		return checkHTTP(p.httpClient, job.target)
	case "tcp":
		s, m := checkTCP(job.target)
		return s, m, nil
	case "ping":
		s, m := checkPing(job.target)
		return s, m, nil
	default:
		return "unknown", fmt.Sprintf("unrecognized monitor type: %s", job.monitorType), nil
	}
}

// checkHTTP performs a GET request; 2xx/3xx is up, anything else (including
// transport errors and timeouts) is down. For HTTPS targets, also returns
// the leaf certificate's expiry so callers can warn ahead of renewal.
func checkHTTP(client *http.Client, target string) (status, message string, certExpiry *time.Time) {
	start := time.Now()
	resp, err := client.Get(target)
	if err != nil {
		return "down", err.Error(), nil
	}
	defer resp.Body.Close()
	rtt := time.Since(start)

	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		notAfter := resp.TLS.PeerCertificates[0].NotAfter
		certExpiry = &notAfter
	}

	if resp.StatusCode < 400 {
		return "up", fmt.Sprintf("HTTP %d in %s", resp.StatusCode, rtt.Round(time.Millisecond)), certExpiry
	}
	return "down", fmt.Sprintf("HTTP %d", resp.StatusCode), certExpiry
}

// checkTCP attempts a TCP connection to target ("host:port"); success is up.
func checkTCP(target string) (string, string) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, monitorCheckTimeout)
	if err != nil {
		return "down", err.Error()
	}
	defer conn.Close()
	return "up", fmt.Sprintf("TCP connect succeeded in %s", time.Since(start).Round(time.Millisecond))
}

// checkPing sends a real ICMP echo request. Requires CAP_NET_RAW, which
// Docker grants by default to containers running as root (Lantern's does).
func checkPing(host string) (string, string) {
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return "down", fmt.Sprintf("icmp socket error: %v", err)
	}
	defer conn.Close()

	dst, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return "down", fmt.Sprintf("dns resolve failed: %v", err)
	}

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("lantern-ping"),
		},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return "down", fmt.Sprintf("failed to build icmp packet: %v", err)
	}

	start := time.Now()
	if _, err := conn.WriteTo(wb, dst); err != nil {
		return "down", fmt.Sprintf("icmp write failed: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(monitorCheckTimeout)); err != nil {
		return "down", fmt.Sprintf("failed to set read deadline: %v", err)
	}
	reply := make([]byte, 1500)
	n, _, err := conn.ReadFrom(reply)
	if err != nil {
		return "down", fmt.Sprintf("no icmp reply: %v", err)
	}
	rtt := time.Since(start)

	rm, err := icmp.ParseMessage(1, reply[:n]) // protocol 1 = ICMPv4
	if err != nil {
		return "down", fmt.Sprintf("failed to parse icmp reply: %v", err)
	}
	if rm.Type != ipv4.ICMPTypeEchoReply {
		return "down", fmt.Sprintf("unexpected icmp reply type: %v", rm.Type)
	}
	return "up", fmt.Sprintf("ping reply in %s", rtt.Round(time.Millisecond))
}

// ---------------------------------------------------------------------------
// Scheduler: one ticker per enabled monitor, submitting jobs to the pool.
// ---------------------------------------------------------------------------

type monitorScheduler struct {
	mu      sync.Mutex
	stopFns map[string]chan struct{}
	pool    *monitorPool
	db      *sql.DB
}

func newMonitorScheduler(db *sql.DB, pool *monitorPool) *monitorScheduler {
	return &monitorScheduler{stopFns: make(map[string]chan struct{}), pool: pool, db: db}
}

// loadAndStartAll starts a ticker for every enabled monitor found in the DB.
// Called once at startup.
func (s *monitorScheduler) loadAndStartAll() {
	rows, err := s.db.Query(`SELECT service_name, monitor_type, target, interval_seconds FROM active_monitors WHERE enabled = 1`)
	if err != nil {
		log.Printf("monitorScheduler: failed to load monitors: %v", err)
		return
	}
	defer rows.Close()

	type m struct {
		name, mtype, target string
		interval            int
	}
	var monitors []m
	for rows.Next() {
		var x m
		if err := rows.Scan(&x.name, &x.mtype, &x.target, &x.interval); err != nil {
			continue
		}
		monitors = append(monitors, x)
	}
	for _, x := range monitors {
		s.start(x.name, x.mtype, x.target, x.interval)
	}
	log.Printf("monitorScheduler: started %d active monitor(s)", len(monitors))
}

// start begins (or restarts, if already running) the ticker for one service.
func (s *monitorScheduler) start(serviceName, monitorType, target string, intervalSeconds int) {
	s.stop(serviceName)

	stop := make(chan struct{})
	s.mu.Lock()
	s.stopFns[serviceName] = stop
	s.mu.Unlock()

	go func() {
		// Run an immediate check on (re)start so status appears right away
		// instead of waiting a full interval.
		s.pool.enqueue(monitorCheckJob{serviceName: serviceName, monitorType: monitorType, target: target})

		ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.pool.enqueue(monitorCheckJob{serviceName: serviceName, monitorType: monitorType, target: target})
			}
		}
	}()
}

// stop halts the ticker for one service, if running. Safe to call when not running.
func (s *monitorScheduler) stop(serviceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.stopFns[serviceName]; ok {
		close(ch)
		delete(s.stopFns, serviceName)
	}
}

// ---------------------------------------------------------------------------
// CRUD handlers
// ---------------------------------------------------------------------------

func validateMonitorTarget(monitorType, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("target is required")
	}
	switch monitorType {
	case "http":
		if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
			return fmt.Errorf("http target must start with http:// or https://")
		}
	case "tcp":
		if _, _, err := net.SplitHostPort(target); err != nil {
			return fmt.Errorf("tcp target must be host:port")
		}
	case "ping":
		// any non-empty hostname/IP is accepted; resolution is validated at check time
	default:
		return fmt.Errorf("monitor_type must be one of: http, tcp, ping")
	}
	return nil
}

// applyCertFields fills CertDaysRemaining/CertWarning from a nullable
// cert_expiry_at column value already scanned into m.CertExpiryAt.
func applyCertFields(m *ActiveMonitor, certExpiry sql.NullString) {
	if !certExpiry.Valid {
		return
	}
	m.CertExpiryAt = &certExpiry.String
	t, err := time.Parse(time.RFC3339, certExpiry.String)
	if err != nil {
		return
	}
	days := int(time.Until(t).Hours() / 24)
	m.CertDaysRemaining = &days
	m.CertWarning = days <= certExpiryWarningDays
}

// handleGetMonitors handles GET /api/monitors — lists every configured active monitor.
func handleGetMonitors(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`SELECT service_name, monitor_type, target, interval_seconds, enabled, last_checked_at, cert_expiry_at FROM active_monitors ORDER BY service_name ASC`)
		if err != nil {
			log.Printf("handleGetMonitors db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()

		monitors := []ActiveMonitor{}
		for rows.Next() {
			var m ActiveMonitor
			var enabled int
			var lastChecked, certExpiry sql.NullString
			if err := rows.Scan(&m.ServiceName, &m.MonitorType, &m.Target, &m.IntervalSeconds, &enabled, &lastChecked, &certExpiry); err != nil {
				continue
			}
			m.Enabled = enabled == 1
			if lastChecked.Valid {
				m.LastCheckedAt = &lastChecked.String
			}
			applyCertFields(&m, certExpiry)
			monitors = append(monitors, m)
		}
		writeJSON(w, http.StatusOK, monitors)
	}
}

// handleGetServiceMonitor handles GET /api/services/{name}/monitor.
func handleGetServiceMonitor(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		var m ActiveMonitor
		var enabled int
		var lastChecked, certExpiry sql.NullString
		err := db.QueryRow(`SELECT service_name, monitor_type, target, interval_seconds, enabled, last_checked_at, cert_expiry_at FROM active_monitors WHERE service_name = ?`, name).
			Scan(&m.ServiceName, &m.MonitorType, &m.Target, &m.IntervalSeconds, &enabled, &lastChecked, &certExpiry)
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "no active monitor configured for this service")
			return
		}
		if err != nil {
			log.Printf("handleGetServiceMonitor db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		m.Enabled = enabled == 1
		if lastChecked.Valid {
			m.LastCheckedAt = &lastChecked.String
		}
		applyCertFields(&m, certExpiry)
		writeJSON(w, http.StatusOK, m)
	}
}

// handlePutServiceMonitor handles PUT /api/services/{name}/monitor — creates or updates.
func handlePutServiceMonitor(db *sql.DB, scheduler *monitorScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(mux.Vars(r)["name"])
		if name == "" {
			writeError(w, http.StatusBadRequest, "service name is required")
			return
		}

		var req struct {
			MonitorType     string `json:"monitor_type"`
			Target          string `json:"target"`
			IntervalSeconds int    `json:"interval_seconds"`
			Enabled         *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json body")
			return
		}

		req.MonitorType = strings.ToLower(strings.TrimSpace(req.MonitorType))
		if !validMonitorTypes[req.MonitorType] {
			writeError(w, http.StatusBadRequest, "monitor_type must be one of: http, tcp, ping")
			return
		}
		if err := validateMonitorTarget(req.MonitorType, req.Target); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.IntervalSeconds == 0 {
			req.IntervalSeconds = 60
		}
		if req.IntervalSeconds < minMonitorIntervalSeconds || req.IntervalSeconds > maxMonitorIntervalSeconds {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("interval_seconds must be between %d and %d", minMonitorIntervalSeconds, maxMonitorIntervalSeconds))
			return
		}
		enabled := true
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		enabledInt := 0
		if enabled {
			enabledInt = 1
		}

		_, err := db.Exec(`
INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled, updated_at)
VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(service_name) DO UPDATE SET
    monitor_type = excluded.monitor_type,
    target = excluded.target,
    interval_seconds = excluded.interval_seconds,
    enabled = excluded.enabled,
    updated_at = CURRENT_TIMESTAMP`,
			name, req.MonitorType, strings.TrimSpace(req.Target), req.IntervalSeconds, enabledInt)
		if err != nil {
			log.Printf("handlePutServiceMonitor db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		if enabled {
			scheduler.start(name, req.MonitorType, strings.TrimSpace(req.Target), req.IntervalSeconds)
		} else {
			scheduler.stop(name)
		}

		writeJSON(w, http.StatusOK, ActiveMonitor{
			ServiceName:     name,
			MonitorType:     req.MonitorType,
			Target:          strings.TrimSpace(req.Target),
			IntervalSeconds: req.IntervalSeconds,
			Enabled:         enabled,
		})
	}
}

// handleDeleteServiceMonitor handles DELETE /api/services/{name}/monitor.
// Removes the active-check config; the service reverts to push-only. Its
// historical status_events are untouched.
func handleDeleteServiceMonitor(db *sql.DB, scheduler *monitorScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		scheduler.stop(name)
		_, err := db.Exec(`DELETE FROM active_monitors WHERE service_name = ?`, name)
		if err != nil {
			log.Printf("handleDeleteServiceMonitor db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service_name": name})
	}
}
