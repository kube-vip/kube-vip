package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdlog "log"
	log "log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// Each invocation needs the original slog default handler and standard logger.
// Subprocesses avoid leaking global logger changes into other tests.
func TestLoggingHelperProcess(t *testing.T) {
	mode := os.Getenv("KUBE_VIP_LOGGING_TEST")
	switch mode {
	case "logs", "klog":
		level, err := strconv.ParseInt(os.Getenv("KUBE_VIP_LOGGING_LEVEL"), 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		configureLogging(&kubevip.Config{
			LogFormat:          os.Getenv("KUBE_VIP_LOGGING_FORMAT"),
			Logging:            int32(level),
			LogTimestampFormat: os.Getenv("KUBE_VIP_LOGGING_TIMESTAMP"),
		})
		if mode == "klog" {
			emitKlogTestRecords()
			os.Exit(0)
		}
		log.Debug("debug record")
		log.Info("info record", "count", 3, "enabled", true, "detail", "first\nsecond \"quoted\"", "err", errors.New("test error"))
		log.Warn("warning record")
		log.Error("error record")
		stdlog.Print("standard log record")
		os.Exit(0)
	case "command":
		for i, arg := range os.Args {
			if arg == "--" {
				kubeVipCmd.SetArgs(os.Args[i+1:])
				os.Exit(Execute())
			}
		}
		t.Fatal("missing command arguments")
	}
}

func emitKlogTestRecords() {
	log.Info("slog alongside klog")
	stdlog.Print("standard log alongside klog")
	klog.InfoS("klog structured info", "count", 3, "enabled", true,
		"detail", "first\nsecond \"quoted\"", "service", klog.KRef("default", "example"))
	klog.ErrorS(errors.New("synthetic API error"), "klog structured error", "retry", true)
	klog.Infof("klog formatted info %s", "example")
	klog.Warningf("klog formatted warning %s", "example")
	klog.Errorf("klog formatted error %s", "example")
	klog.Background().WithValues("component", "reflector").Info("klog background info")
	klog.TODO().Error(errors.New("synthetic background error"), "klog background error")
	klog.NewKlogr().Info("klogr info")
	logger := klog.Background().WithValues("namespace", "default")
	ctx := klog.NewContext(context.Background(), logger)
	klog.FromContext(ctx).Info("klog contextual info", "ready", true)
	klog.Background().V(4).Info("klog debug record")
	klog.Background().V(5).Info("klog trace record")
	klog.V(0).InfoS("klog legacy verbosity zero")
	klog.V(1).InfoS("klog legacy verbosity one")
	// Constructing a reflector does not start it or make any API requests.
	reflector := cache.NewReflectorWithOptions(&cache.ListWatch{}, &corev1.Service{},
		cache.NewStore(cache.MetaNamespaceKeyFunc), cache.ReflectorOptions{Name: "json-logging-test"})
	cache.DefaultWatchErrorHandler(ctx, reflector, errors.New("synthetic watch failure"))
	klog.Flush()
}

func TestKlogJSONLogging(t *testing.T) {
	for _, level := range []int{-4, 0, 4, 8} {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			stdout, stderr, err := runLoggingProcess(t, "klog", map[string]string{
				"KUBE_VIP_LOGGING_FORMAT": "json", "KUBE_VIP_LOGGING_LEVEL": strconv.Itoa(level),
			})
			if err != nil || stdout != "" {
				t.Fatalf("klog process: err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			records := decodeLogRecords(t, stderr)
			byMessage := map[string][]map[string]any{}
			for _, record := range records {
				message, ok := record["msg"].(string)
				if !ok {
					t.Fatalf("missing log message: %#v", record)
				}
				byMessage[message] = append(byMessage[message], record)
			}
			wantMessages := map[string]int{
				"slog alongside klog": 0, "standard log alongside klog": 0,
				"klog structured info": 0, "klog structured error": 8,
				"klog formatted info example": 0, "klog formatted error example": 8,
				// The official klog/logr bridge maps legacy Warning calls to Info.
				"klog formatted warning example": 0,
				"klog background info":           0, "klog background error": 8,
				"klogr info": 0, "klog contextual info": 0,
				"klog debug record": -4, "klog trace record": -5,
				"klog legacy verbosity zero": 0, "Failed to watch": 8,
			}
			for message, severity := range wantMessages {
				wantCount := 0
				if severity >= level {
					wantCount = 1
				}
				found := byMessage[message]
				if len(found) != wantCount {
					t.Fatalf("got %d records for %q at level %d, want %d: %s", len(found), message, level, wantCount, stderr)
				}
				if len(found) == 1 {
					if found[0]["level"] != log.Level(severity).String() {
						t.Errorf("level for %q = %v, want %s", message, found[0]["level"], log.Level(severity))
					}
				}
			}
			// client-go can emit additional feature-gate debug records; every line
			// was decoded above, while expected records must not be duplicated.
			if len(byMessage["klog legacy verbosity one"]) != 0 {
				t.Fatalf("legacy verbosity gate changed: %s", stderr)
			}
			if watch := byMessage["Failed to watch"][0]; watch["err"] != "synthetic watch failure" || watch["namespace"] != "default" || watch["reflector"] != "json-logging-test" {
				t.Fatalf("lost client-go error or context: %#v", watch)
			}
			if level <= 0 {
				info := byMessage["klog structured info"][0]
				if info["count"] != float64(3) || info["enabled"] != true || info["detail"] != "first\nsecond \"quoted\"" {
					t.Fatalf("lost structured klog attributes: %#v", info)
				}
				service, ok := info["service"].(map[string]any)
				if !ok || service["name"] != "example" || service["namespace"] != "default" {
					t.Fatalf("lost structured service reference: %#v", info["service"])
				}
				if context := byMessage["klog contextual info"][0]; context["namespace"] != "default" || context["ready"] != true {
					t.Fatalf("lost contextual logger attributes: %#v", context)
				}
			}
			if level == 0 {
				t.Logf("Actual JSON output (synthetic events, no cluster):\n%s", stderr)
			}
		})
	}
}

func TestKlogTextLoggingUnchanged(t *testing.T) {
	for _, format := range []string{"", "text"} {
		t.Run(format, func(t *testing.T) {
			stdout, stderr, err := runLoggingProcess(t, "klog", map[string]string{
				"KUBE_VIP_LOGGING_FORMAT": format, "KUBE_VIP_LOGGING_LEVEL": "0",
			})
			if err != nil || stdout != "" {
				t.Fatalf("klog process: err=%v stdout=%q stderr=%q", err, stdout, stderr)
			}
			if !strings.Contains(stderr, `] "klog structured info"`) || !strings.Contains(stderr, "] klog formatted warning example") || !strings.Contains(stderr, `] "Failed to watch"`) || strings.Contains(stderr, `"level":`) {
				t.Fatalf("legacy klog text output changed: %s", stderr)
			}
		})
	}
}

func runLoggingProcess(t *testing.T, mode string, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestLoggingHelperProcess$", "--"}, args...)...)
	// Do not let the developer's kube-vip settings affect this subprocess.
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "GORACE=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "KUBE_VIP_LOGGING_TEST="+mode)
	// os.Exit bypasses the test runner's coverage teardown in the helper.
	// Give instrumented subprocesses a directory instead of emitting a warning.
	command.Env = append(command.Env, "GOCOVERDIR="+t.TempDir())
	for name, value := range env {
		command.Env = append(command.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatalf("logging subprocess timed out: %v", ctx.Err())
	}
	return stdout.String(), stderr.String(), err
}

func decodeLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	return decodeLogRecordsWithTimestamp(t, output, "rfc3339")
}

func decodeLogRecordsWithTimestamp(t *testing.T, output, format string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("not a JSON log line: %q: %v", line, err)
		}
		if format == "unix-ms" {
			timestamp, ok := record["time"].(float64)
			if !ok || timestamp != float64(int64(timestamp)) {
				t.Fatalf("log timestamp is not an integer JSON number: %#v", record["time"])
			}
		} else {
			timestamp, ok := record["time"].(string)
			if !ok {
				t.Fatalf("missing log timestamp: %#v", record)
			}
			if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
				t.Fatalf("invalid log timestamp: %v", err)
			}
		}
		records = append(records, record)
	}
	return records
}

func TestJSONLogging(t *testing.T) {
	stdout, stderr, err := runLoggingProcess(t, "logs", map[string]string{
		"KUBE_VIP_LOGGING_FORMAT": "json", "KUBE_VIP_LOGGING_LEVEL": "-4",
	})
	if err != nil || stdout != "" {
		t.Fatalf("logging process: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	records := decodeLogRecords(t, stderr)
	if len(records) != 5 {
		t.Fatalf("got %d log records, want 5: %s", len(records), stderr)
	}
	info := records[1]
	if info["level"] != "INFO" || info["msg"] != "info record" || info["count"] != float64(3) || info["enabled"] != true || info["detail"] != "first\nsecond \"quoted\"" || info["err"] != "test error" {
		t.Fatalf("lost JSON attributes: %#v", info)
	}
	if records[4]["msg"] != "standard log record" || records[4]["level"] != "INFO" {
		t.Fatalf("standard log was not bridged: %#v", records[4])
	}
}

func TestLoggingLevels(t *testing.T) {
	formats := []struct {
		name      string
		format    string
		timestamp string
	}{
		{name: "default"},
		{name: "text", format: "text"},
		{name: "json", format: "json", timestamp: "rfc3339"},
		{name: "json-unix-ms", format: "json", timestamp: "unix-ms"},
	}
	for _, format := range formats {
		for _, level := range []int{-4, 0, 4, 8} {
			t.Run(format.name+"/"+strconv.Itoa(level), func(t *testing.T) {
				stdout, stderr, err := runLoggingProcess(t, "logs", map[string]string{
					"KUBE_VIP_LOGGING_FORMAT": format.format, "KUBE_VIP_LOGGING_LEVEL": strconv.Itoa(level),
					"KUBE_VIP_LOGGING_TIMESTAMP": format.timestamp,
				})
				if err != nil || stdout != "" {
					t.Fatalf("logging process: err=%v stdout=%q stderr=%q", err, stdout, stderr)
				}
				for message, severity := range map[string]int{"debug record": -4, "info record": 0, "warning record": 4, "error record": 8} {
					if strings.Contains(stderr, message) != (severity >= level) {
						t.Errorf("wrong filtering for %q at level %d: %s", message, level, stderr)
					}
				}
				// Standard log calls have no severity. JSON assigns INFO and applies
				// the handler threshold; legacy text output remains unfiltered.
				wantStandard := format.format != "json" || level <= 0
				if strings.Contains(stderr, "standard log record") != wantStandard {
					t.Errorf("wrong standard log filtering at level %d: %s", level, stderr)
				}
				if format.format == "json" {
					for _, record := range decodeLogRecordsWithTimestamp(t, stderr, format.timestamp) {
						if record["msg"] == "standard log record" && record["level"] != "INFO" {
							t.Errorf("standard log severity depends on the threshold: %#v", record)
						}
					}
				} else if !strings.Contains(stderr, " ERROR error record") || strings.Contains(stderr, `"level":`) {
					t.Fatalf("legacy text format changed: %s", stderr)
				}
			})
		}
	}
}

func TestRuntimeCommandJSONLogging(t *testing.T) {
	for _, name := range []string{"manager", "service"} {
		for _, source := range []string{"flag", "environment", "file"} {
			t.Run(name+"/"+source, func(t *testing.T) {
				args := []string{name, "--routingProtocol=256", "--prometheusHTTPServer="}
				env := map[string]string{}
				switch source {
				case "flag":
					args = append(args, "--logFormat=json")
				case "environment":
					env["vip_logformat"] = "json"
				case "file":
					args = append(args, "--config-file="+writeRuntimeConfig(t, "logFormat: json\n"))
				}
				stdout, stderr, err := runLoggingProcess(t, "command", env, args...)
				if err == nil || stdout != "" {
					t.Fatalf("expected startup failure: err=%v stdout=%q stderr=%q", err, stdout, stderr)
				}
				records := decodeLogRecords(t, stderr)
				if len(records) != 1 || records[0]["level"] != "ERROR" || !strings.Contains(records[0]["err"].(string), "routingProtocol") {
					t.Fatalf("startup error did not use JSON: %s", stderr)
				}
			})
		}
	}
}

func TestManagerFirstStartupLogIsJSON(t *testing.T) {
	stdout, stderr, err := runLoggingProcess(t, "command", nil, "manager", "--logFormat=json", "--prometheusHTTPServer=")
	if err == nil || stdout != "" {
		t.Fatalf("expected missing-mode failure: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	records := decodeLogRecords(t, stderr)
	if len(records) != 2 || records[0]["msg"] != "kube-vip.io" || records[1]["msg"] != "command failed" {
		t.Fatalf("unexpected startup logs: %s", stderr)
	}
}

func TestRuntimeCommandRejectsInvalidLogFormat(t *testing.T) {
	for _, name := range []string{"manager", "service"} {
		t.Run(name, func(t *testing.T) {
			_, stderr, err := runLoggingProcess(t, "command", nil, name, "--logFormat=yaml", "--prometheusHTTPServer=")
			if err == nil || !strings.Contains(stderr, "logFormat") || !strings.Contains(stderr, "expected text or json") {
				t.Fatalf("invalid log format was not rejected: err=%v stderr=%q", err, stderr)
			}
		})
	}
}

func TestJSONLogFormatDoesNotChangeCommandOutput(t *testing.T) {
	stdout, stderr, err := runLoggingProcess(t, "command", nil, "version", "--logFormat=json", "--logTimestampFormat=unix-ms")
	if err != nil || stderr != "" || !strings.HasPrefix(stdout, "Kube-VIP Release Information\n") {
		t.Fatalf("version output changed: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	stdout, stderr, err = runLoggingProcess(t, "command", nil, "manifest", "pod", "--interface=eth0", "--arp", "--services", "--logFormat=json", "--logTimestampFormat=unix-ms")
	if err != nil || stderr != "" {
		t.Fatalf("manifest output changed: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal([]byte(stdout), &pod); err != nil {
		t.Fatalf("manifest is not valid YAML: %v", err)
	}
	if pod.Kind != "Pod" || len(pod.Spec.Containers) != 1 {
		t.Fatalf("unexpected Pod manifest: %s", stdout)
	}
	loggingEnv := map[string]string{}
	for _, env := range pod.Spec.Containers[0].Env {
		loggingEnv[env.Name] = env.Value
	}
	if loggingEnv["vip_logformat"] != "json" || loggingEnv["vip_logtimestampformat"] != "unix-ms" {
		t.Fatal("JSON format and timestamp selection were not propagated to the generated Pod")
	}
}
