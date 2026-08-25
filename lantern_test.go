package main

import "testing"

// ---------------------------------------------------------------------------
// Heartbeat window: padding, ordering, uptime ratio
// ---------------------------------------------------------------------------

func TestLeftPadEmptyBeats(t *testing.T) {
	t.Run("nil pads to full width", func(t *testing.T) {
		got := leftPadEmptyBeats(nil, 30)
		if len(got) != 30 {
			t.Fatalf("len = %d, want 30", len(got))
		}
		for i, b := range got {
			if b.Status != "empty" {
				t.Errorf("beat %d = %q, want empty", i, b.Status)
			}
		}
	})

	t.Run("pads on the left and keeps real beats newest-last", func(t *testing.T) {
		real := []HeartbeatBeat{
			{Status: "up", Msg: "a"},
			{Status: "down", Msg: "b"},
			{Status: "up", Msg: "c"},
		}
		got := leftPadEmptyBeats(real, 30)
		if len(got) != 30 {
			t.Fatalf("len = %d, want 30", len(got))
		}
		for i := 0; i < 27; i++ {
			if got[i].Status != "empty" {
				t.Errorf("beat %d = %q, want empty padding", i, got[i].Status)
			}
		}
		// Real beats must survive in order, occupying the newest slots.
		if got[27].Msg != "a" || got[28].Msg != "b" || got[29].Msg != "c" {
			t.Errorf("tail = %q/%q/%q, want a/b/c", got[27].Msg, got[28].Msg, got[29].Msg)
		}
	})

	t.Run("full window is returned untouched", func(t *testing.T) {
		full := make([]HeartbeatBeat, 30)
		for i := range full {
			full[i] = HeartbeatBeat{Status: "up"}
		}
		if got := leftPadEmptyBeats(full, 30); len(got) != 30 {
			t.Fatalf("len = %d, want 30", len(got))
		}
	})
}

func TestWindowUptimePct(t *testing.T) {
	beats := func(statuses ...string) []HeartbeatBeat {
		out := make([]HeartbeatBeat, 0, len(statuses))
		for _, s := range statuses {
			out = append(out, HeartbeatBeat{Status: s})
		}
		return out
	}

	cases := []struct {
		name string
		in   []HeartbeatBeat
		want float64
	}{
		{"nil window is zero, not NaN", nil, 0},
		{"all padding is zero, not NaN", beats("empty", "empty", "empty"), 0},
		{"all up", beats("up", "up", "up", "up"), 100},
		{"all down", beats("down", "down"), 0},
		{"half up", beats("up", "down"), 50},
		{"padding excluded from both sides", beats("empty", "empty", "up", "up"), 100},
		{"partial window scored on real checks only", beats("empty", "up", "up", "down"), 200.0 / 3.0},
		{"degraded counts against uptime but stays in denominator", beats("up", "degraded"), 50},
		{"maintenance is not up", beats("up", "maintenance"), 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowUptimePct(tc.in)
			if diff := got - tc.want; diff > 0.0001 || diff < -0.0001 {
				t.Errorf("windowUptimePct = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Webhook flap dampening
// ---------------------------------------------------------------------------

func TestShouldNotify(t *testing.T) {
	cases := []struct {
		name     string
		prev2    string
		prev1    string
		current  string
		wantFire bool
		wantKind string
	}{
		{"first ever event is a baseline", "", "", "up", false, ""},
		{"first ever event, down, is still a baseline", "", "", "down", false, ""},
		{"steady up is silent", "up", "up", "up", false, ""},
		{"first down is held back", "up", "up", "down", false, ""},
		{"second consecutive down fires once", "up", "down", "down", true, "down"},
		{"third down does not re-alert", "down", "down", "down", false, ""},
		{"recovery after announced outage fires", "down", "down", "up", true, "recovery"},
		{"single-beat flap is fully silent", "up", "down", "up", false, ""},
		{"new service confirmed down fires", "", "down", "down", true, "down"},
		{"leaving confirmed down to degraded is a recovery", "down", "down", "degraded", true, "recovery"},
		{"up to degraded reports immediately", "up", "up", "degraded", true, "change"},
		{"degraded to up reports immediately", "up", "degraded", "up", true, "change"},
		{"steady degraded is silent", "degraded", "degraded", "degraded", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fire, kind := shouldNotify(tc.prev2, tc.prev1, tc.current)
			if fire != tc.wantFire || kind != tc.wantKind {
				t.Errorf("shouldNotify(%q,%q,%q) = (%v,%q), want (%v,%q)",
					tc.prev2, tc.prev1, tc.current, fire, kind, tc.wantFire, tc.wantKind)
			}
		})
	}
}

// TestShouldNotifyOutageProducesExactlyOnePair walks a realistic outage and
// asserts the whole episode yields one DOWN and one recovery, not a stream.
func TestShouldNotifyOutageProducesExactlyOnePair(t *testing.T) {
	seq := []string{"up", "up", "down", "down", "down", "down", "up", "up"}

	var downs, recoveries int
	for i := 2; i < len(seq); i++ {
		fire, kind := shouldNotify(seq[i-2], seq[i-1], seq[i])
		if !fire {
			continue
		}
		switch kind {
		case "down":
			downs++
		case "recovery":
			recoveries++
		default:
			t.Errorf("unexpected kind %q at index %d", kind, i)
		}
	}
	if downs != 1 || recoveries != 1 {
		t.Errorf("got %d down / %d recovery alerts, want exactly 1 each", downs, recoveries)
	}
}

// ---------------------------------------------------------------------------
// SVG badge colours
// ---------------------------------------------------------------------------

func TestBadgeColorForStatus(t *testing.T) {
	cases := map[string]string{
		"up":          "#10B981",
		"down":        "#F43F5E",
		"degraded":    "#F59E0B",
		"maintenance": "#6B7280",
		"unknown":     "#6B7280",
		"":            "#6B7280",
		"nonsense":    "#6B7280",
	}
	for status, want := range cases {
		if got := badgeColorForStatus(status); got != want {
			t.Errorf("badgeColorForStatus(%q) = %s, want %s", status, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Native Docker discovery mapping
// ---------------------------------------------------------------------------

func TestDockerHealthFromStatus(t *testing.T) {
	cases := map[string]string{
		"Up 4 days (healthy)":             "healthy",
		"Up 2 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 4 days":                       "",
		"Exited (0) 4 hours ago":          "",
		"":                                "",
	}
	for status, want := range cases {
		if got := dockerHealthFromStatus(status); got != want {
			t.Errorf("dockerHealthFromStatus(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestDockerStatusFor(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		status string
		want   string
	}{
		{"running healthy", "running", "Up 4 days (healthy)", "up"},
		{"running with no healthcheck is taken at its word", "running", "Up 4 days", "up"},
		{"running but still warming up is not yet up", "running", "Up 5 seconds (health: starting)", "degraded"},
		{"running but unhealthy", "running", "Up 2 minutes (unhealthy)", "degraded"},
		{"restarting", "restarting", "Restarting (1) 5 seconds ago", "degraded"},
		{"paused", "paused", "Up 2 days (Paused)", "degraded"},
		{"exited", "exited", "Exited (0) 4 hours ago", "down"},
		{"dead", "dead", "Dead", "down"},
		{"created but never started", "created", "Created", "down"},
		{"removing", "removing", "Removal In Progress", "down"},
		{"unrecognised state", "teleporting", "Who knows", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := dockerStatusFor(tc.state, tc.status)
			if got != tc.want {
				t.Errorf("dockerStatusFor(%q,%q) = %q, want %q", tc.state, tc.status, got, tc.want)
			}
			// The Docker status line is already human-readable and is carried
			// through verbatim as the beat message.
			if msg != tc.status {
				t.Errorf("message = %q, want passthrough %q", msg, tc.status)
			}
		})
	}

	t.Run("empty status falls back to the raw state", func(t *testing.T) {
		if _, msg := dockerStatusFor("running", ""); msg != "state: running" {
			t.Errorf("message = %q, want %q", msg, "state: running")
		}
	})
}

func TestDockerServiceName(t *testing.T) {
	t.Run("strips the leading slash", func(t *testing.T) {
		got := dockerServiceName(DockerContainerSummary{Names: []string{"/lantern"}})
		if got != "lantern" {
			t.Errorf("got %q, want lantern", got)
		}
	})

	t.Run("skips empty names", func(t *testing.T) {
		got := dockerServiceName(DockerContainerSummary{Names: []string{"", "/redis"}})
		if got != "redis" {
			t.Errorf("got %q, want redis", got)
		}
	})

	t.Run("falls back to a short container id", func(t *testing.T) {
		got := dockerServiceName(DockerContainerSummary{ID: "abcdef1234567890fedcba"})
		if got != "abcdef123456" {
			t.Errorf("got %q, want abcdef123456", got)
		}
	})
}

func TestDockerDiscoveryIgnored(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"nil labels", nil, false},
		{"label absent", map[string]string{"com.docker.compose.service": "web"}, false},
		{"opted out", map[string]string{"lantern.ignore": "true"}, true},
		{"opted out, odd casing", map[string]string{"lantern.ignore": "TRUE"}, true},
		{"opted out, padded", map[string]string{"lantern.ignore": "  true  "}, true},
		{"explicitly opted in", map[string]string{"lantern.ignore": "false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerDiscoveryIgnored(tc.labels); got != tc.want {
				t.Errorf("dockerDiscoveryIgnored(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}
