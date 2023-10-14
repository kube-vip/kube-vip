//go:build e2e
// +build e2e

package e2e

import (
	"k8s.io/klog/v2"
	"sigs.k8s.io/kind/pkg/log"
)

type TestLogger struct{}

func (t TestLogger) Warnf(format string, args ...interface{}) {
	klog.Warningf(format, args...)
}

func (t TestLogger) Warn(message string) {
	klog.Warning(message)
}

func (t TestLogger) Error(message string) {
	klog.Error(message)
}

func (t TestLogger) Errorf(format string, args ...interface{}) {
	klog.Errorf(format, args...)
}

func (t TestLogger) Printf(format string, args ...interface{}) {
	klog.Infof(format, args...)
}

func (t TestLogger) V(level log.Level) log.InfoLogger {
	return TestInfoLogger{Verbose: klog.V(klog.Level(level))}
}

type TestInfoLogger struct {
	klog.Verbose
}

func (t TestInfoLogger) Info(message string) {
	t.Verbose.Info(message)
}
