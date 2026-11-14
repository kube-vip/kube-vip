package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	log "log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func TestJSONLogTimestampEncoding(t *testing.T) {
	tests := []struct {
		name      string
		timestamp time.Time
		wantMilli int64
	}{
		{name: "fractional UTC", timestamp: time.Unix(1792060107, 65987654).UTC(), wantMilli: 1792060107065},
		{name: "same instant with offset", timestamp: time.Unix(1792060107, 65987654).In(time.FixedZone("UTC+0530", 19800)), wantMilli: 1792060107065},
		{name: "epoch", timestamp: time.Unix(0, 0).UTC()},
		{name: "before epoch", timestamp: time.Unix(-1, 999999999).UTC(), wantMilli: -1},
		{name: "submillisecond precision", timestamp: time.Unix(1, 1999999).UTC(), wantMilli: 1001},
		{name: "zero time omitted"},
	}
	for _, format := range []string{"", "rfc3339", "unix-ms"} {
		for _, tt := range tests {
			t.Run(format+"/"+tt.name, func(t *testing.T) {
				var output bytes.Buffer
				handler := newJSONLogHandler(&output, &kubevip.Config{LogTimestampFormat: format})
				record := log.NewRecord(tt.timestamp, log.LevelInfo, "timestamp test", 0)
				record.AddAttrs(log.Time("observed_at", tt.timestamp), log.Group("nested", log.Time("time", tt.timestamp)))
				if err := handler.Handle(context.Background(), record); err != nil {
					t.Fatal(err)
				}
				encoded := output.String()
				decoder := json.NewDecoder(strings.NewReader(encoded))
				decoder.UseNumber()
				var got map[string]any
				if err := decoder.Decode(&got); err != nil {
					t.Fatal(err)
				}
				timestamp, found := got["time"]
				switch {
				case tt.timestamp.IsZero():
					if found {
						t.Fatalf("zero record time should be omitted, got %v", timestamp)
					}
				case format == "unix-ms":
					if timestamp != json.Number(strconv.FormatInt(tt.wantMilli, 10)) {
						t.Fatalf("timestamp = %#v, want integer %d", timestamp, tt.wantMilli)
					}
				default:
					if timestamp != tt.timestamp.Format(time.RFC3339Nano) {
						t.Fatalf("RFC3339 timestamp changed: %v", timestamp)
					}
				}
				if _, renamed := got["timestamp"]; renamed {
					t.Fatal("the time field must not be renamed")
				}
				if got["observed_at"] != tt.timestamp.Format(time.RFC3339Nano) {
					t.Fatalf("unrelated time attribute changed: %v", got["observed_at"])
				}
				nested, ok := got["nested"].(map[string]any)
				if !ok || nested["time"] != tt.timestamp.Format(time.RFC3339Nano) {
					t.Fatalf("nested time attribute changed: %#v", got["nested"])
				}
				if tt.name == "fractional UTC" && format != "" {
					t.Logf("Actual encoded record: %s", encoded)
				}
			})
		}
	}
}

func TestJSONTimestampPreservesBoundGroups(t *testing.T) {
	var output bytes.Buffer
	fixed := time.Unix(1792060107, 65000000).UTC()
	handler := newJSONLogHandler(&output, &kubevip.Config{LogTimestampFormat: "unix-ms"}).
		WithAttrs([]log.Attr{log.String("component", "test")}).WithGroup("request").
		WithAttrs([]log.Attr{log.Time("time", fixed)})
	record := log.NewRecord(fixed, log.LevelInfo, "grouped timestamp test", 0)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got := decodeLogRecordsWithTimestamp(t, output.String(), "unix-ms")[0]
	group, ok := got["request"].(map[string]any)
	if !ok || group["time"] != fixed.Format(time.RFC3339Nano) || got["component"] != "test" {
		t.Fatalf("bound group or attributes changed: %#v", got)
	}
}

func TestJSONTimestampPreservesStringAttribute(t *testing.T) {
	var output bytes.Buffer
	handler := newJSONLogHandler(&output, &kubevip.Config{LogTimestampFormat: "unix-ms"})
	// With no built-in record time, a string attribute named time is still valid.
	record := log.NewRecord(time.Time{}, log.LevelInfo, "string attribute test", 0)
	record.AddAttrs(log.String("time", "application-provided value"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["time"] != "application-provided value" {
		t.Fatalf("string attribute was rewritten: %#v", got)
	}
}

func TestKlogUnixMillisTimestamp(t *testing.T) {
	for _, level := range []int{-4, 0, 4, 8} {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			stdout, stderr, err := runLoggingProcess(t, "klog", map[string]string{
				"KUBE_VIP_LOGGING_FORMAT": "json", "KUBE_VIP_LOGGING_LEVEL": strconv.Itoa(level),
				"KUBE_VIP_LOGGING_TIMESTAMP": "unix-ms",
			})
			if err != nil || stdout != "" {
				t.Fatalf("klog process: err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			foundWatch, foundDebug, foundInfo := false, false, false
			for _, record := range decodeLogRecordsWithTimestamp(t, stderr, "unix-ms") {
				switch record["msg"] {
				case "Failed to watch":
					foundWatch = record["err"] == "synthetic watch failure"
				case "klog debug record":
					foundDebug = true
				case "klog structured info":
					foundInfo = true
				}
			}
			if !foundWatch || foundDebug != (level <= -4) || foundInfo != (level <= 0) {
				t.Fatalf("timestamp option changed klog filtering or lost the client-go error: %s", stderr)
			}
			if level == 0 {
				t.Logf("Actual klog JSON with numeric timestamps (synthetic events):\n%s", stderr)
			}
		})
	}
}

func TestLogTimestampIgnoredForText(t *testing.T) {
	for _, format := range []string{"", "text"} {
		t.Run(format, func(t *testing.T) {
			stdout, stderr, err := runLoggingProcess(t, "klog", map[string]string{
				"KUBE_VIP_LOGGING_FORMAT": format, "KUBE_VIP_LOGGING_LEVEL": "0",
				"KUBE_VIP_LOGGING_TIMESTAMP": "unix-ms",
			})
			if err != nil || stdout != "" || !strings.Contains(stderr, " INFO slog alongside klog") || !strings.Contains(stderr, `] "klog structured info"`) || strings.Contains(stderr, `"level":`) {
				t.Fatalf("timestamp option changed text logging: err=%v stderr=%q", err, stderr)
			}
		})
	}
}

func TestRuntimeCommandLogTimestampFormat(t *testing.T) {
	for _, name := range []string{"manager", "service"} {
		for _, source := range []string{"flag", "environment", "file"} {
			for _, format := range []string{"rfc3339", "unix-ms"} {
				t.Run(name+"/"+source+"/"+format, func(t *testing.T) {
					args := []string{name, "--logFormat=json", "--routingProtocol=256", "--prometheusHTTPServer="}
					env := map[string]string{}
					switch source {
					case "flag":
						args = append(args, "--logTimestampFormat="+format)
					case "environment":
						env["vip_logtimestampformat"] = format
					case "file":
						args = append(args, "--config-file="+writeRuntimeConfig(t, "logTimestampFormat: "+format+"\n"))
					}
					before := time.Now().UnixMilli()
					stdout, stderr, err := runLoggingProcess(t, "command", env, args...)
					if err == nil || stdout != "" {
						t.Fatalf("expected validation failure: err=%v stdout=%q stderr=%q", err, stdout, stderr)
					}
					records := decodeLogRecordsWithTimestamp(t, stderr, format)
					if len(records) != 1 || !strings.Contains(records[0]["err"].(string), "routingProtocol") {
						t.Fatalf("unexpected startup error: %s", stderr)
					}
					if format == "unix-ms" {
						stamp := int64(records[0]["time"].(float64))
						if stamp < before || stamp > time.Now().UnixMilli() {
							t.Fatalf("timestamp %d is not the current epoch in milliseconds", stamp)
						}
					}
				})
			}
		}
	}
}

func TestRuntimeCommandRejectsInvalidTimestampFormat(t *testing.T) {
	for _, name := range []string{"manager", "service"} {
		t.Run(name, func(t *testing.T) {
			_, stderr, err := runLoggingProcess(t, "command", nil, name, "--logFormat=json", "--logTimestampFormat=unix", "--prometheusHTTPServer=")
			if err == nil || !strings.Contains(stderr, "logTimestampFormat") || !strings.Contains(stderr, "expected rfc3339 or unix-ms") {
				t.Fatalf("invalid timestamp format was not rejected: err=%v stderr=%q", err, stderr)
			}
		})
	}
}
