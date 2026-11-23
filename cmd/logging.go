package cmd

import (
	"io"
	log "log/slog"
	"os"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"k8s.io/klog/v2"
)

// configureLogging runs once after runtime configuration has been resolved,
// before validation and startup logs. Text mode retains Go's default handler
// so existing text output is unchanged.
func configureLogging(config *kubevip.Config) {
	if config.LogFormat != "json" {
		log.SetLogLoggerLevel(log.Level(config.Logging))
		return
	}
	// After SetDefault, SetLogLoggerLevel assigns a severity to standard log
	// calls instead of setting a threshold. Keep those calls at INFO and let
	// the JSON handler apply the configured threshold.
	log.SetLogLoggerLevel(log.LevelInfo)
	log.SetDefault(log.New(newJSONLogHandler(os.Stderr, config)))
	// Route legacy and contextual Kubernetes library logs to the same handler.
	klog.SetSlogLogger(log.Default())
}

func newJSONLogHandler(output io.Writer, config *kubevip.Config) *log.JSONHandler {
	options := &log.HandlerOptions{Level: log.Level(config.Logging)}
	if config.LogTimestampFormat == "unix-ms" {
		options.ReplaceAttr = func(groups []string, attr log.Attr) log.Attr {
			if len(groups) == 0 && attr.Key == log.TimeKey && attr.Value.Kind() == log.KindTime {
				attr.Value = log.Int64Value(attr.Value.Time().UnixMilli())
			}
			return attr
		}
	}
	return log.NewJSONHandler(output, options)
}
