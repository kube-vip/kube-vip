//go:build e2e
// +build e2e

package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
)

// ScrapeMetrics gets and parses the Prometheus metrics exposed by kube-vip in
// a Kind node. nodeName may be either a Kind node suffix or its full container
// name.
func ScrapeMetrics(clusterName, nodeName string) (map[string]float64, error) {
	if clusterName == "" {
		return nil, fmt.Errorf("cluster name is empty")
	}
	if nodeName == "" {
		return nil, fmt.Errorf("node name is empty")
	}

	containerName := nodeName
	if !strings.HasPrefix(nodeName, clusterName+"-") {
		containerName = fmt.Sprintf("%s-%s", clusterName, nodeName)
	}

	cmd := exec.Command("docker", "exec", containerName, "curl", "-fsS", "http://127.0.0.1:2112/metrics")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	payload, err := cmd.Output()
	if err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("scrape metrics from %s: %w: %s", containerName, err, detail)
		}
		return nil, fmt.Errorf("scrape metrics from %s: %w", containerName, err)
	}

	metrics, err := parseMetrics(string(payload))
	if err != nil {
		return nil, fmt.Errorf("parse metrics from %s: %w", containerName, err)
	}
	return metrics, nil
}

// MetricValue returns a metric whose labels contain all requested labels and
// the number of matching series. When matches is greater than one, value is
// the first matching series in sorted key order and must not be used as a
// single-series value.
func MetricValue(metrics map[string]float64, name string, labels map[string]string) (float64, int) {
	values := matchingMetricValues(metrics, name, labels)
	if len(values) == 0 {
		return 0, 0
	}
	return values[0], len(values)
}

// SumMetric returns the sum of all series whose labels contain all requested
// labels. It returns zero when no series matches.
func SumMetric(metrics map[string]float64, name string, labels map[string]string) float64 {
	var total float64
	for _, value := range matchingMetricValues(metrics, name, labels) {
		total += value
	}
	return total
}

// MaxMetric returns the maximum value among all series whose labels contain
// all requested labels. The bool is false when no series matches.
func MaxMetric(metrics map[string]float64, name string, labels map[string]string) (float64, bool) {
	values := matchingMetricValues(metrics, name, labels)
	if len(values) == 0 {
		return 0, false
	}

	maximum := values[0]
	for _, value := range values[1:] {
		if value > maximum {
			maximum = value
		}
	}
	return maximum, true
}

func matchingMetricValues(metrics map[string]float64, name string, labels map[string]string) []float64 {
	keys := make([]string, 0, len(metrics))
	for key := range metrics {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	values := make([]float64, 0)
	for _, key := range keys {
		metricName, metricLabels, ok := parseMetricKey(key)
		if !ok || metricName != name {
			continue
		}

		matches := true
		for labelName, labelValue := range labels {
			metricLabelValue, exists := metricLabels[labelName]
			if !exists || metricLabelValue != labelValue {
				matches = false
				break
			}
		}
		if matches {
			values = append(values, metrics[key])
		}
	}

	return values
}

// CounterDelta returns the difference between two counter samples. Missing
// samples are treated as unavailable rather than as a counter reset.
func CounterDelta(before, after map[string]float64, name string, labels map[string]string) (float64, bool) {
	beforeValue, beforeMatches := MetricValue(before, name, labels)
	afterValue, afterMatches := MetricValue(after, name, labels)
	if beforeMatches != 1 || afterMatches != 1 {
		return 0, false
	}
	return afterValue - beforeValue, true
}

// EventuallyMetric retries a metric scrape until its value satisfies matcher.
func EventuallyMetric(clusterName, node, name string, labels map[string]string, matcher types.GomegaMatcher, timeout, interval time.Duration) {
	Eventually(func() (float64, error) {
		metrics, err := ScrapeMetrics(clusterName, node)
		if err != nil {
			return 0, err
		}

		return singleMetricValue(metrics, name, labels)
	}, timeout, interval).Should(matcher)
}

// ConsistentlyMetric repeatedly scrapes a metric while its value satisfies
// matcher.
func ConsistentlyMetric(clusterName, node, name string, labels map[string]string, matcher types.GomegaMatcher, timeout, interval time.Duration) {
	Consistently(func() (float64, error) {
		metrics, err := ScrapeMetrics(clusterName, node)
		if err != nil {
			return 0, err
		}

		return singleMetricValue(metrics, name, labels)
	}, timeout, interval).Should(matcher)
}

// MetricStable returns a metric value after confirming that it is unchanged
// across samples separated by gap. The observed window is (samples-1)*gap
// plus scrape latency. A single transient scrape error is retried once for
// each sample.
func MetricStable(clusterName, node, name string, labels map[string]string, samples int, gap time.Duration) (float64, error) {
	if samples < 2 {
		return 0, fmt.Errorf("samples must be at least 2")
	}

	var stableValue float64
	for sample := 0; sample < samples; sample++ {
		metrics, err := scrapeMetricsRetryOnce(clusterName, node)
		if err != nil {
			return 0, err
		}

		value, err := singleMetricValue(metrics, name, labels)
		if err != nil {
			return 0, err
		}
		if sample == 0 {
			stableValue = value
		} else if value != stableValue {
			return 0, fmt.Errorf("metric %s with labels %v changed from %v to %v", name, labels, stableValue, value)
		}

		if sample+1 < samples {
			time.Sleep(gap)
		}
	}

	return stableValue, nil
}

func singleMetricValue(metrics map[string]float64, name string, labels map[string]string) (float64, error) {
	value, matches := MetricValue(metrics, name, labels)
	switch matches {
	case 0:
		return 0, fmt.Errorf("metric %s with labels %v was not found", name, labels)
	case 1:
		return value, nil
	default:
		return 0, fmt.Errorf("metric %s with labels %v matched %d series", name, labels, matches)
	}
}

func scrapeMetricsRetryOnce(clusterName, node string) (map[string]float64, error) {
	metrics, err := ScrapeMetrics(clusterName, node)
	if err == nil {
		return metrics, nil
	}

	retryMetrics, retryErr := ScrapeMetrics(clusterName, node)
	if retryErr == nil {
		return retryMetrics, nil
	}
	return nil, fmt.Errorf("scrape metrics from %s failed: %v; retry failed: %w", node, err, retryErr)
}

func parseMetrics(payload string) (map[string]float64, error) {
	metrics := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(payload))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, labels, value, err := parseMetricSample(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		key := canonicalMetricKey(name, labels)
		if _, exists := metrics[key]; exists {
			return nil, fmt.Errorf("line %d: duplicate sample %s", lineNumber, key)
		}
		metrics[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan payload: %w", err)
	}

	return metrics, nil
}

func parseMetricSample(line string) (string, map[string]string, float64, error) {
	nameEnd := 0
	for nameEnd < len(line) && isMetricNameChar(line[nameEnd], nameEnd == 0) {
		nameEnd++
	}
	if nameEnd == 0 {
		return "", nil, 0, fmt.Errorf("invalid metric name")
	}

	name := line[:nameEnd]
	position := nameEnd
	labels := map[string]string{}
	if position < len(line) && line[position] == '{' {
		labelEnd, err := closingBrace(line, position+1)
		if err != nil {
			return "", nil, 0, err
		}
		labels, err = parseLabelSet(line[position+1 : labelEnd])
		if err != nil {
			return "", nil, 0, err
		}
		position = labelEnd + 1
	}

	if position >= len(line) || !isWhitespace(line[position]) {
		return "", nil, 0, fmt.Errorf("metric %s is missing a value", name)
	}
	for position < len(line) && isWhitespace(line[position]) {
		position++
	}
	valueStart := position
	for position < len(line) && !isWhitespace(line[position]) {
		position++
	}
	if valueStart == position {
		return "", nil, 0, fmt.Errorf("metric %s is missing a value", name)
	}

	value, err := strconv.ParseFloat(line[valueStart:position], 64)
	if err != nil {
		return "", nil, 0, fmt.Errorf("invalid value for metric %s: %w", name, err)
	}
	return name, labels, value, nil
}

func parseMetricKey(key string) (string, map[string]string, bool) {
	brace := strings.IndexByte(key, '{')
	if brace < 0 {
		if !validMetricName(key) {
			return "", nil, false
		}
		return key, map[string]string{}, true
	}
	if !strings.HasSuffix(key, "}") || brace == 0 {
		return "", nil, false
	}

	name := key[:brace]
	if !validMetricName(name) {
		return "", nil, false
	}
	labels, err := parseLabelSet(key[brace+1 : len(key)-1])
	if err != nil {
		return "", nil, false
	}
	return name, labels, true
}

func canonicalMetricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}

	labelNames := make([]string, 0, len(labels))
	for labelName := range labels {
		labelNames = append(labelNames, labelName)
	}
	sort.Strings(labelNames)

	var builder strings.Builder
	builder.WriteString(name)
	builder.WriteByte('{')
	for index, labelName := range labelNames {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(labelName)
		builder.WriteByte('=')
		builder.WriteString(strconv.Quote(labels[labelName]))
	}
	builder.WriteByte('}')
	return builder.String()
}

func closingBrace(line string, start int) (int, error) {
	inQuotes := false
	escaped := false
	for position := start; position < len(line); position++ {
		character := line[position]
		if inQuotes {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inQuotes = false
			}
			continue
		}

		switch character {
		case '"':
			inQuotes = true
		case '}':
			return position, nil
		}
	}
	return 0, fmt.Errorf("unterminated label set")
}

func parseLabelSet(labelSet string) (map[string]string, error) {
	labels := make(map[string]string)
	position := 0
	for {
		for position < len(labelSet) && isWhitespace(labelSet[position]) {
			position++
		}
		if position == len(labelSet) {
			return labels, nil
		}

		labelStart := position
		for position < len(labelSet) && isMetricNameChar(labelSet[position], position == labelStart) {
			position++
		}
		if labelStart == position {
			return nil, fmt.Errorf("invalid label name")
		}
		labelName := labelSet[labelStart:position]

		for position < len(labelSet) && isWhitespace(labelSet[position]) {
			position++
		}
		if position == len(labelSet) || labelSet[position] != '=' {
			return nil, fmt.Errorf("label %s is missing an equals sign", labelName)
		}
		position++
		for position < len(labelSet) && isWhitespace(labelSet[position]) {
			position++
		}
		if position == len(labelSet) || labelSet[position] != '"' {
			return nil, fmt.Errorf("label %s is missing a quoted value", labelName)
		}

		valueStart := position
		position++
		escaped := false
		closed := false
		for position < len(labelSet) {
			character := labelSet[position]
			position++
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == '"' {
				closed = true
				break
			}
		}
		if !closed {
			return nil, fmt.Errorf("label %s has an unterminated value", labelName)
		}

		value, err := strconv.Unquote(labelSet[valueStart:position])
		if err != nil {
			return nil, fmt.Errorf("invalid value for label %s: %w", labelName, err)
		}
		if _, exists := labels[labelName]; exists {
			return nil, fmt.Errorf("duplicate label %s", labelName)
		}
		labels[labelName] = value

		for position < len(labelSet) && isWhitespace(labelSet[position]) {
			position++
		}
		if position == len(labelSet) {
			return labels, nil
		}
		if labelSet[position] != ',' {
			return nil, fmt.Errorf("expected comma after label %s", labelName)
		}
		position++
	}
}

func validMetricName(name string) bool {
	if name == "" {
		return false
	}
	for position := range name {
		if !isMetricNameChar(name[position], position == 0) {
			return false
		}
	}
	return true
}

func isMetricNameChar(character byte, first bool) bool {
	if character == '_' || character == ':' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' {
		return true
	}
	return !first && character >= '0' && character <= '9'
}

func isWhitespace(character byte) bool {
	return character == ' ' || character == '\t'
}
