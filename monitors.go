// Active monitoring (Phase 2): optional Uptime-Kuma-style checks that Lantern
// performs itself (HTTP, TCP, ICMP ping) on a schedule, as an alternative or
// supplement to services pushing their own status via POST /api/status.
// Results are written through the same ingestStatusEvent() path push updates
// use, so uptime %, incidents, and the history graph work identically
// regardless of which mechanism produced the event.
package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

var validMonitorTypes = map[string]bool{"http": true, "tcp": true, "ping": true}

// bodyPatternCache holds compiled body_pattern regexes, keyed by the pattern
// string itself. handlePutServiceMonitor already validates every pattern via
// regexp.Compile before it's persisted, so checkHTTP re-compiling the same
// pattern on every check tick (every interval_seconds, for every HTTP monitor
// that uses this field) is pure repeated work — cache it instead, mirroring
// the caching already done for the more expensive uptime-metrics path
// (getCachedOrComputeServiceMetrics in extensions.go). Keying by pattern
// string rather than service name means a changed pattern is simply a cache
// miss under its new key; stale entries under abandoned keys are harmless and
// bounded by the number of distinct patterns ever configured.
var (
	bodyPatternCacheMu sync.RWMutex
	bodyPatternCache   = make(map[string]*regexp.Regexp)
)

// compiledBodyPattern returns a compiled regexp for pattern, using the cache
// when possible. Errors are only expected here in practice if a pattern was
// persisted before validation existed, or written directly to the DB.
func compiledBodyPattern(pattern string) (*regexp.Regexp, error) {
	bodyPatternCacheMu.RLock()
	re, ok := bodyPatternCache[pattern]
	bodyPatternCacheMu.RUnlock()
	if ok {
		return re, nil
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	bodyPatternCacheMu.Lock()
	bodyPatternCache[pattern] = re
	bodyPatternCacheMu.Unlock()
	return re, nil
}

const (
	minMonitorIntervalSeconds = 10
	maxMonitorIntervalSeconds = 3600
	monitorCheckTimeout       = 10 * time.Second
	monitorQueueSize          = 256
	// maxHTTPCheckBody caps how much of an HTTP monitor's response body is
	// read for body_pattern/json_path evaluation, mirroring maxDiagnosticsBody's
	// defensive cap on request bodies elsewhere in this codebase.
	maxHTTPCheckBody = 1 << 20 // 1 MiB
)

// Certificate expiry thresholds, in days. Set from Config at startup; the
// defaults here are what applies before loadConfig runs and in tests.
var (
	certWarnDays     = 30
	certCriticalDays = 7
)

// Certificate lifecycle states, coarsest first.
const (
	certStatusOK       = "ok"
	certStatusWarning  = "warning"
	certStatusCritical = "critical"
	certStatusExpired  = "expired"
)

// certStatusFor classifies how much validity a certificate has left.
//
// Negative days means it has already expired, which is materially different
// from "expiring soon": clients are failing TLS verification right now.
func certStatusFor(daysRemaining int) string {
	switch {
	case daysRemaining < 0:
		return certStatusExpired
	case daysRemaining <= certCriticalDays:
		return certStatusCritical
	case daysRemaining <= certWarnDays:
		return certStatusWarning
	default:
		return certStatusOK
	}
}

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
	// CertWarning is kept for compatibility with existing consumers: true for
	// anything that is not "ok". CertStatus is the useful one.
	CertWarning bool   `json:"cert_warning"`
	CertStatus  string `json:"cert_status,omitempty"`
	// BodyPattern, JSONPath and JSONExpect are optional "http" monitor checks
	// layered on top of the status-code check: nil means that check is not
	// configured. See checkHTTP.
	BodyPattern *string `json:"body_pattern,omitempty"`
	JSONPath    *string `json:"json_path,omitempty"`
	JSONExpect  *string `json:"json_expect,omitempty"`
}

// ---------------------------------------------------------------------------
// Worker pool: executes checks concurrently, never sequentially/blocking.
// ---------------------------------------------------------------------------

type monitorCheckJob struct {
	serviceName string
	monitorType string
	target      string
	// BodyPattern, JSONPath and JSONExpect mirror ActiveMonitor's fields of
	// the same name: nil means that check is not configured for this run.
	BodyPattern *string `json:"body_pattern,omitempty"`
	JSONPath    *string `json:"json_path,omitempty"`
	JSONExpect  *string `json:"json_expect,omitempty"`
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
		// Time the probe itself. Measured around runCheck only, so the recorded
		// latency is the network check and excludes the DB writes below.
		start := time.Now()
		status, message, certExpiry := p.runCheck(job)
		latencyMs := time.Since(start).Milliseconds()

		if certExpiry != nil {
			daysLeft := int(time.Until(*certExpiry).Hours() / 24)
			on := certExpiry.Format("2006-01-02")

			switch certStatusFor(daysLeft) {
			case certStatusExpired:
				// The endpoint answered us because Lantern's own client is not
				// strict about it, but a browser is: verification fails for
				// real users, so this is not a healthy service.
				status = "down"
				message = fmt.Sprintf("%s — TLS certificate EXPIRED %d day(s) ago (%s)", message, -daysLeft, on)
			case certStatusCritical:
				// Days from expiry is an outage with a start date already set.
				// Degrading here is what turns it into one alert now rather
				// than a surprise later.
				if status == "up" {
					status = "degraded"
				}
				message = fmt.Sprintf("%s — TLS certificate expires in %d day(s) (%s)", message, daysLeft, on)
			case certStatusWarning:
				message = fmt.Sprintf("%s — TLS certificate expires in %d day(s) (%s)", message, daysLeft, on)
			}
		}

		ingestStatusEvent(p.db, p.cfg, p.dispatcher, p.hub, job.serviceName, status, message, time.Now().UTC(), latencyMs)

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
		bodyPattern, jsonPath, jsonExpect := "", "", ""
		if job.BodyPattern != nil {
			bodyPattern = *job.BodyPattern
		}
		if job.JSONPath != nil {
			jsonPath = *job.JSONPath
		}
		if job.JSONExpect != nil {
			jsonExpect = *job.JSONExpect
		}
		return checkHTTP(p.httpClient, job.target, bodyPattern, jsonPath, jsonExpect)
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

// checkHTTP performs a GET request. The status-code check runs first, exactly
// as before: anything other than 2xx/3xx (including transport errors and
// timeouts) is down, regardless of bodyPattern/jsonPath. For HTTPS targets,
// it also returns the leaf certificate's expiry so callers can warn ahead of
// renewal.
//
// bodyPattern and jsonPath are additional, optional checks layered on top of
// the status-code check; an empty string means that check is not configured.
// When bodyPattern is non-empty, the response body must match it as a regexp.
// When jsonPath is non-empty, the JSON field it selects (dotted path, per
// gjson's syntax) must equal jsonExpect. All configured checks must pass for
// "up"; the first one that fails determines the "down" message.
func checkHTTP(client *http.Client, target, bodyPattern, jsonPath, jsonExpect string) (status, message string, certExpiry *time.Time) {
	start := time.Now()
	resp, err := client.Get(target)
	if err != nil {
		return "down", err.Error(), nil
	}
	defer resp.Body.Close()

	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		notAfter := resp.TLS.PeerCertificates[0].NotAfter
		certExpiry = &notAfter
	}

	if resp.StatusCode >= 400 {
		return "down", fmt.Sprintf("HTTP %d", resp.StatusCode), certExpiry
	}

	// Only read the body when a check actually needs it — the common case
	// (status-code check only) shouldn't pay for draining the response.
	var body []byte
	if bodyPattern != "" || jsonPath != "" {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxHTTPCheckBody))
		if err != nil {
			return "down", fmt.Sprintf("failed to read response body: %v", err), certExpiry
		}
	}

	if bodyPattern != "" {
		re, err := compiledBodyPattern(bodyPattern)
		if err != nil {
			return "down", fmt.Sprintf("invalid body_pattern regex: %v", err), certExpiry
		}
		if !re.Match(body) {
			return "down", "body pattern mismatch", certExpiry
		}
	}

	if jsonPath != "" {
		got := gjson.GetBytes(body, jsonPath).String()
		if got != jsonExpect {
			return "down", fmt.Sprintf("json path %s = %s, want %s", jsonPath, got, jsonExpect), certExpiry
		}
	}

	rtt := time.Since(start)
	return "up", fmt.Sprintf("HTTP %d in %s", resp.StatusCode, rtt.Round(time.Millisecond)), certExpiry
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

// pingSeq hands out a distinct sequence number per probe. A raw ICMP socket
// receives every echo reply the host gets, not just the ones answering this
// probe, so concurrent checks need something to tell their own reply apart.
var pingSeq atomic.Uint32

// checkPing sends a real ICMP echo request. Requires CAP_NET_RAW, which
// Docker grants by default to containers running as root (Lantern's does).
//
// The reply is matched on source address, echo ID and sequence number. Without
// that, four workers pinging four hosts concurrently could each read whichever
// reply arrived first — so a check against a dead host would report "up" on the
// strength of a live host's reply. Unmatched packets are skipped rather than
// treated as the answer, until the deadline expires.
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

	id := os.Getpid() & 0xffff
	seq := int(pingSeq.Add(1) & 0xffff)

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   id,
			Seq:  seq,
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
	for {
		n, peer, err := conn.ReadFrom(reply)
		if err != nil {
			return "down", fmt.Sprintf("no icmp reply: %v", err)
		}
		rtt := time.Since(start)

		// Someone else's echo reply, arriving on our shared raw socket.
		if peer == nil || peer.String() != dst.IP.String() {
			continue
		}

		rm, err := icmp.ParseMessage(1, reply[:n]) // protocol 1 = ICMPv4
		if err != nil {
			continue
		}
		if rm.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok || echo.ID != id || echo.Seq != seq {
			continue
		}
		return "up", fmt.Sprintf("ping reply in %s", rtt.Round(time.Millisecond))
	}
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
	rows, err := s.db.Query(`SELECT service_name, monitor_type, target, interval_seconds, body_pattern, json_path, json_expect FROM active_monitors WHERE enabled = 1`)
	if err != nil {
		log.Printf("monitorScheduler: failed to load monitors: %v", err)
		return
	}
	defer rows.Close()

	type m struct {
		name, mtype, target               string
		interval                          int
		bodyPattern, jsonPath, jsonExpect sql.NullString
	}
	var monitors []m
	for rows.Next() {
		var x m
		if err := rows.Scan(&x.name, &x.mtype, &x.target, &x.interval, &x.bodyPattern, &x.jsonPath, &x.jsonExpect); err != nil {
			continue
		}
		monitors = append(monitors, x)
	}
	for _, x := range monitors {
		s.start(x.name, x.mtype, x.target, x.interval, nullStringPtr(x.bodyPattern), nullStringPtr(x.jsonPath), nullStringPtr(x.jsonExpect))
	}
	log.Printf("monitorScheduler: started %d active monitor(s)", len(monitors))
}

// start begins (or restarts, if already running) the ticker for one service.
//
// The stop-and-replace is done under a single lock hold. Splitting it — stop(),
// release, re-acquire, store — left a window in which two concurrent
// PUT /api/services/{name}/monitor calls both created a channel and one
// overwrote the other in the map. The overwritten goroutine's stop channel was
// then unreachable, so its ticker ran for the lifetime of the process, still
// enqueuing checks and still writing status_events. The visible symptom was a
// service that reappeared after being deleted.
func (s *monitorScheduler) start(serviceName, monitorType, target string, intervalSeconds int, bodyPattern, jsonPath, jsonExpect *string) {
	stop := make(chan struct{})

	s.mu.Lock()
	if prev, ok := s.stopFns[serviceName]; ok {
		close(prev)
		delete(s.stopFns, serviceName)
	}
	s.stopFns[serviceName] = stop
	s.mu.Unlock()

	go func() {
		// Run an immediate check on (re)start so status appears right away
		// instead of waiting a full interval.
		s.pool.enqueue(monitorCheckJob{serviceName: serviceName, monitorType: monitorType, target: target, BodyPattern: bodyPattern, JSONPath: jsonPath, JSONExpect: jsonExpect})

		ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.pool.enqueue(monitorCheckJob{serviceName: serviceName, monitorType: monitorType, target: target, BodyPattern: bodyPattern, JSONPath: jsonPath, JSONExpect: jsonExpect})
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
	m.CertStatus = certStatusFor(days)
	m.CertWarning = m.CertStatus != certStatusOK
}

// nullStringPtr converts a scanned nullable TEXT column to *string: nil for
// SQL NULL, a pointer to the value otherwise. Used for body_pattern,
// json_path and json_expect, whose NULL means "no check configured".
func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// handleGetMonitors handles GET /api/monitors — lists every configured active monitor.
func handleGetMonitors(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`SELECT service_name, monitor_type, target, interval_seconds, enabled, last_checked_at, cert_expiry_at, body_pattern, json_path, json_expect FROM active_monitors ORDER BY service_name ASC`)
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
			var lastChecked, certExpiry, bodyPattern, jsonPath, jsonExpect sql.NullString
			if err := rows.Scan(&m.ServiceName, &m.MonitorType, &m.Target, &m.IntervalSeconds, &enabled, &lastChecked, &certExpiry, &bodyPattern, &jsonPath, &jsonExpect); err != nil {
				continue
			}
			m.Enabled = enabled == 1
			if lastChecked.Valid {
				m.LastCheckedAt = &lastChecked.String
			}
			applyCertFields(&m, certExpiry)
			m.BodyPattern = nullStringPtr(bodyPattern)
			m.JSONPath = nullStringPtr(jsonPath)
			m.JSONExpect = nullStringPtr(jsonExpect)
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
		var lastChecked, certExpiry, bodyPattern, jsonPath, jsonExpect sql.NullString
		err := db.QueryRow(`SELECT service_name, monitor_type, target, interval_seconds, enabled, last_checked_at, cert_expiry_at, body_pattern, json_path, json_expect FROM active_monitors WHERE service_name = ?`, name).
			Scan(&m.ServiceName, &m.MonitorType, &m.Target, &m.IntervalSeconds, &enabled, &lastChecked, &certExpiry, &bodyPattern, &jsonPath, &jsonExpect)
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
		m.BodyPattern = nullStringPtr(bodyPattern)
		m.JSONPath = nullStringPtr(jsonPath)
		m.JSONExpect = nullStringPtr(jsonExpect)
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

		scopedSvc := r.Context().Value(scopedServiceKey)
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

		var req struct {
			MonitorType     string  `json:"monitor_type"`
			Target          string  `json:"target"`
			IntervalSeconds int     `json:"interval_seconds"`
			Enabled         *bool   `json:"enabled"`
			BodyPattern     *string `json:"body_pattern"`
			JSONPath        *string `json:"json_path"`
			JSONExpect      *string `json:"json_expect"`
		}
		if !decodeJSONBody(w, r, maxConfigBody, &req) {
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
		// json_path/json_expect need no special validation: gjson degrades
		// gracefully, so a missing key at check time is simply a non-match,
		// not an error. body_pattern is a regexp, though, and a bad one
		// would otherwise only surface as a mysterious "down" at check time.
		if req.BodyPattern != nil && *req.BodyPattern != "" {
			if _, err := regexp.Compile(*req.BodyPattern); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid body_pattern regex: %v", err))
				return
			}
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
INSERT INTO active_monitors (service_name, monitor_type, target, interval_seconds, enabled, body_pattern, json_path, json_expect, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
ON CONFLICT(service_name) DO UPDATE SET
    monitor_type = excluded.monitor_type,
    target = excluded.target,
    interval_seconds = excluded.interval_seconds,
    enabled = excluded.enabled,
    body_pattern = excluded.body_pattern,
    json_path = excluded.json_path,
    json_expect = excluded.json_expect,
    updated_at = CURRENT_TIMESTAMP`,
			name, req.MonitorType, strings.TrimSpace(req.Target), req.IntervalSeconds, enabledInt, req.BodyPattern, req.JSONPath, req.JSONExpect)
		if err != nil {
			log.Printf("handlePutServiceMonitor db error: %v", err)
			writeError(w, http.StatusInternalServerError, "database error")
			return
		}

		if enabled {
			scheduler.start(name, req.MonitorType, strings.TrimSpace(req.Target), req.IntervalSeconds, req.BodyPattern, req.JSONPath, req.JSONExpect)
		} else {
			scheduler.stop(name)
		}

		writeJSON(w, http.StatusOK, ActiveMonitor{
			ServiceName:     name,
			MonitorType:     req.MonitorType,
			Target:          strings.TrimSpace(req.Target),
			IntervalSeconds: req.IntervalSeconds,
			Enabled:         enabled,
			BodyPattern:     req.BodyPattern,
			JSONPath:        req.JSONPath,
			JSONExpect:      req.JSONExpect,
		})
	}
}

// handleDeleteServiceMonitor handles DELETE /api/services/{name}/monitor.
// Removes the active-check config; the service reverts to push-only. Its
// historical status_events are untouched.
func handleDeleteServiceMonitor(db *sql.DB, scheduler *monitorScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSpace(mux.Vars(r)["name"])

		scopedSvc := r.Context().Value(scopedServiceKey)
		if scopedSvc != nil && scopedSvc.(string) != name {
			writeError(w, http.StatusForbidden, "token not scoped for this service")
			return
		}

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
