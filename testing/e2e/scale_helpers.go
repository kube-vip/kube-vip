//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/lease"
)

const (
	scalePollInterval         = time.Second
	scaleSuiteLabel           = "scale.kube-vip.io/suite"
	scaleSuiteValue           = "pr13"
	scaleScenarioLabel        = "scale.kube-vip.io/scenario"
	scaleBackendLabel         = "scale.kube-vip.io/backend"
	scaleBackendName          = "shared-backend"
	scaleBackendContainerName = "whoami"
	scaleBackendImage         = "ghcr.io/traefik/whoami:v1.11"
	scaleBackendPort          = 80
	scaleRevisionAnnotation   = "scale.kube-vip.io/revision"
)

type ScaleMetricSnapshot map[string]map[string]float64

func ValidateScaleTopology(controlPlaneNodes, maxNodes int) error {
	if controlPlaneNodes != maxNodes {
		return fmt.Errorf("scale topology has %d control-plane nodes, want the bounded maximum of %d", controlPlaneNodes, maxNodes)
	}
	if controlPlaneNodes < 3 {
		return fmt.Errorf("scale topology needs at least three control-plane nodes, got %d", controlPlaneNodes)
	}
	return nil
}

func BuildScaleClient(config *rest.Config, qps float32, burst int) (kubernetes.Interface, error) {
	if config == nil {
		return nil, fmt.Errorf("scale client config is nil")
	}
	if qps <= 0 || burst <= 0 {
		return nil, fmt.Errorf("scale client QPS and burst must be positive: qps=%v burst=%d", qps, burst)
	}

	clientConfig := rest.CopyConfig(config)
	clientConfig.QPS = qps
	clientConfig.Burst = burst
	clientConfig.Timeout = 15 * time.Second

	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("create scale Kubernetes client: %w", err)
	}
	return client, nil
}

func EnsureScaleNamespace(ctx context.Context, client kubernetes.Interface, namespace string) error {
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create namespace %q: %w", namespace, err)
	}
	return nil
}

func scaleBackendLabels() map[string]string {
	return map[string]string{
		scaleSuiteLabel:   scaleSuiteValue,
		scaleBackendLabel: scaleBackendName,
	}
}

func CreateScaleBackend(ctx context.Context, client kubernetes.Interface, namespace string, replicas int32) error {
	labelsForBackend := scaleBackendLabels()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scaleBackendName,
			Namespace: namespace,
			Labels:    labelsForBackend,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labelsForBackend},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelsForBackend},
				Spec: corev1.PodSpec{
					Tolerations: scaleControlPlaneTolerations(),
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       "kubernetes.io/hostname",
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: labelsForBackend},
					}},
					Containers: []corev1.Container{{
						Name:  scaleBackendContainerName,
						Image: scaleBackendImage,
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: scaleBackendPort,
							Protocol:      corev1.ProtocolTCP,
						}},
					}},
				},
			},
		},
	}

	if _, err := client.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create shared backend deployment: %w", err)
	}
	return nil
}

func scaleControlPlaneTolerations() []corev1.Toleration {
	return []corev1.Toleration{
		{
			Key:      "node-role.kubernetes.io/control-plane",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
		{
			Key:      "node-role.kubernetes.io/master",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}
}

func ScaleBackend(ctx context.Context, client kubernetes.Interface, namespace string, replicas int32) error {
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, scaleBackendName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get shared backend deployment: %w", err)
	}
	deployment.Spec.Replicas = &replicas
	if _, err := client.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scale shared backend deployment to %d: %w", replicas, err)
	}
	return nil
}

func ScaleBackendReady(ctx context.Context, client kubernetes.Interface, namespace string, replicas int32) error {
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, scaleBackendName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get shared backend deployment status: %w", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != replicas {
		return fmt.Errorf("shared backend desired replicas are %v, want %d", deployment.Spec.Replicas, replicas)
	}
	if deployment.Status.UpdatedReplicas != replicas ||
		deployment.Status.ReadyReplicas != replicas ||
		deployment.Status.AvailableReplicas != replicas {
		return fmt.Errorf("shared backend is not ready at %d replicas: updated=%d ready=%d available=%d",
			replicas,
			deployment.Status.UpdatedReplicas,
			deployment.Status.ReadyReplicas,
			deployment.Status.AvailableReplicas,
		)
	}
	return nil
}

func scaleBackendReadyNodes(ctx context.Context, client kubernetes.Interface, namespace string) (map[string]struct{}, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(scaleBackendLabels()).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list shared backend pods: %w", err)
	}

	readyNodes := make(map[string]struct{})
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Spec.NodeName == "" || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if ready {
			readyNodes[pod.Spec.NodeName] = struct{}{}
		}
	}

	return readyNodes, nil
}

func CreateScaleService(ctx context.Context, client kubernetes.Interface, namespace, scenario, name, vip string,
	trafficPolicy corev1.ServiceExternalTrafficPolicy, forcePerServiceElection bool, leaseName string,
) (*corev1.Service, error) {
	service := NewScaleService(namespace, scenario, name, vip, trafficPolicy, forcePerServiceElection, leaseName)
	created, err := client.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create service %s/%s: %w", namespace, name, err)
	}
	return created, nil
}

func NewScaleService(namespace, scenario, name, vip string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	forcePerServiceElection bool, leaseName string,
) *corev1.Service {
	annotations := map[string]string{
		kubevip.LoadbalancerIPAnnotation: vip,
	}
	if forcePerServiceElection {
		annotations[kubevip.ForcePerServiceElection] = "true"
	}
	if leaseName != "" {
		annotations[kubevip.ServiceLease] = leaseName
	}

	serviceLabels := map[string]string{
		scaleSuiteLabel:    scaleSuiteValue,
		scaleScenarioLabel: scenario,
	}
	policy := corev1.IPFamilyPolicySingleStack
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      serviceLabels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy:         trafficPolicy,
			IPFamilies:                    []corev1.IPFamily{corev1.IPv4Protocol},
			IPFamilyPolicy:                &policy,
			Ports:                         []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: scaleBackendPort, TargetPort: intstr.FromInt(int(scaleBackendPort))}},
			Selector:                      scaleBackendLabels(),
			AllocateLoadBalancerNodePorts: boolPtr(true),
		},
	}
}

func ScaleServiceLeaseNames(services []*corev1.Service, namespace string) ([]string, error) {
	names := make([]string, 0, len(services))
	seen := make(map[string]struct{}, len(services))
	for _, service := range services {
		leaseNamespace, leaseName := lease.ServiceName(service)
		if leaseNamespace != namespace {
			return nil, fmt.Errorf("service %s/%s uses lease namespace %q, want %q", service.Namespace, service.Name, leaseNamespace, namespace)
		}
		if _, exists := seen[leaseName]; exists {
			return nil, fmt.Errorf("service %s/%s reuses lease %s/%s", service.Namespace, service.Name, namespace, leaseName)
		}
		seen[leaseName] = struct{}{}
		names = append(names, leaseName)
	}
	return names, nil
}

func boolPtr(value bool) *bool {
	return &value
}

func UpdateScaleService(ctx context.Context, client kubernetes.Interface, namespace, name string, revision int) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		service, err := client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if service.Annotations == nil {
			service.Annotations = make(map[string]string)
		}
		service.Annotations[scaleRevisionAnnotation] = strconv.Itoa(revision)
		_, err = client.CoreV1().Services(namespace).Update(ctx, service, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("update service %s/%s: %w", namespace, name, err)
	}
	return nil
}

func DeleteScaleServices(ctx context.Context, client kubernetes.Interface, namespace, scenario string) error {
	services, err := ListScaleServices(ctx, client, namespace, scenario)
	if err != nil {
		return err
	}
	for _, service := range services {
		err := client.CoreV1().Services(namespace).Delete(ctx, service.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete service %s/%s: %w", namespace, service.Name, err)
		}
	}
	return nil
}

func ListScaleServices(ctx context.Context, client kubernetes.Interface, namespace, scenario string) ([]corev1.Service, error) {
	selector := labels.Set{
		scaleSuiteLabel:    scaleSuiteValue,
		scaleScenarioLabel: scenario,
	}.String()
	services, err := client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list %s scale services: %w", scenario, err)
	}
	return services.Items, nil
}

func WaitForScaleServiceCount(ctx context.Context, client kubernetes.Interface, namespace, scenario string, expected int) error {
	services, err := ListScaleServices(ctx, client, namespace, scenario)
	if err != nil {
		return err
	}
	if len(services) != expected {
		return fmt.Errorf("%s service count is %d, want %d", scenario, len(services), expected)
	}
	return nil
}

func WaitForScaleMetrics(ctx context.Context, clusterName string, nodes []string) error {
	for _, node := range nodes {
		if _, err := ScrapeMetrics(ctx, clusterName, node); err != nil {
			return fmt.Errorf("scrape metrics from node %q: %w", node, err)
		}
	}
	return nil
}

func WaitForScaleServiceContenders(ctx context.Context, clusterName string, nodes []string, namespace string,
	serviceNames []string, minimum int, timeout time.Duration,
) ([]string, error) {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		snapshot, err := SnapshotScaleMetrics(ctx, clusterName, nodes)
		if err == nil {
			contenders, validationErr := scaleServiceContenders(snapshot, namespace, serviceNames, minimum)
			if validationErr == nil {
				return contenders, nil
			}
			lastError = validationErr
		} else {
			lastError = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("service contenders did not become ready: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return nil, err
		}
	}
}

func scaleServiceContenders(snapshot ScaleMetricSnapshot, namespace string, serviceNames []string, minimum int) ([]string, error) {
	if minimum < 1 {
		return nil, fmt.Errorf("minimum contender count must be positive")
	}
	contenders := make([]string, 0, len(snapshot))
	for node, nodeMetrics := range snapshot {
		ready := true
		for _, serviceName := range serviceNames {
			labelsForService := map[string]string{"namespace": namespace, "name": serviceName}
			loops, loopMatches := MetricValue(nodeMetrics, "kube_vip_service_election_loops", labelsForService)
			_, attemptMatches := MetricValue(nodeMetrics, "kube_vip_service_election_attempts_total", labelsForService)
			if loopMatches != 1 || loops != 1 || attemptMatches != 1 {
				ready = false
				break
			}
		}
		if ready {
			contenders = append(contenders, node)
		}
	}
	sort.Strings(contenders)
	if len(contenders) < minimum {
		return nil, fmt.Errorf("only %d nodes have live election loops and attempts for services %v, want at least %d", len(contenders), serviceNames, minimum)
	}
	return contenders, nil
}

func ScaleControlPlaneAvailable(ctx context.Context, client kubernetes.Interface, namespace, leaseName string) error {
	if _, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("read namespace while control-plane kubelet is stopped: %w", err)
	}
	if _, err := client.CoordinationV1().Leases(namespace).Get(ctx, leaseName, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("read service lease while control-plane kubelet is stopped: %w", err)
	}
	return nil
}

func ScaleControlPlaneComponentsRunning(clusterName, node string) error {
	for _, component := range []string{"kube-apiserver", "etcd"} {
		output, err := runDockerOutput(clusterName, "exec", node, "crictl", "ps", "--state", "Running", "--name", component, "-q")
		if err != nil {
			return fmt.Errorf("inspect %s on node %q: %w", component, node, err)
		}
		if strings.TrimSpace(output) == "" {
			return fmt.Errorf("%s is not running on node %q while kubelet is stopped", component, node)
		}
	}
	return nil
}

func ScaleActiveServices(ctx context.Context, clusterName string, nodes []string, namespace string) (float64, bool, error) {
	maximum := 0.0
	found := false
	for _, node := range nodes {
		metrics, err := ScrapeMetrics(ctx, clusterName, node)
		if err != nil {
			return 0, false, err
		}
		value, matches := MetricValue(metrics, "kube_vip_active_services", map[string]string{"namespace": namespace})
		if matches > 1 {
			return 0, false, fmt.Errorf("active-services metric on node %q matched %d series", node, matches)
		}
		found = found || matches == 1
		if matches == 1 && value > maximum {
			maximum = value
		}
	}
	return maximum, found, nil
}

func WaitForScaleActiveServices(ctx context.Context, clusterName string, nodes []string, namespace string, expected int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		active, found, err := ScaleActiveServices(ctx, clusterName, nodes, namespace)
		if err == nil {
			if found && active == float64(expected) {
				return nil
			}
			lastError = fmt.Errorf("active service metric found=%t value=%.0f, want %d", found, active, expected)
		} else {
			lastError = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("active service count did not converge: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return err
		}
	}
}

func WaitForScaleVIPs(ctx context.Context, clusterName string, nodes []string, vips []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		allAdvertised := true
		for _, vip := range vips {
			owners := 0
			for _, node := range nodes {
				if CheckIPAddressPresence(vip, node, true) {
					owners++
				}
			}
			if owners != 1 {
				allAdvertised = false
				lastError = fmt.Errorf("VIP %q has %d owners, want exactly one", vip, owners)
				break
			}
		}
		if allAdvertised {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("VIP spot checks did not converge: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return err
		}
	}
}

func WaitForScaleVIPOwner(ctx context.Context, clusterName string, nodes []string, vip, expectedOwner string,
	requireSoleOwner bool, timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		owners := make([]string, 0, len(nodes))
		for _, node := range nodes {
			if CheckIPAddressPresence(vip, node, true) {
				owners = append(owners, node)
			}
		}
		if err := validateScaleVIPOwners(vip, expectedOwner, owners, requireSoleOwner); err == nil {
			return nil
		} else {
			lastError = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("VIP ownership did not converge: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return err
		}
	}
}

func validateScaleVIPOwners(vip, expectedOwner string, owners []string, requireSoleOwner bool) error {
	for _, owner := range owners {
		if owner != expectedOwner {
			continue
		}
		if !requireSoleOwner || len(owners) == 1 {
			return nil
		}
		return fmt.Errorf("VIP %q is advertised by %v, want sole owner %q", vip, owners, expectedOwner)
	}
	return fmt.Errorf("VIP %q is advertised by %v, want owner %q", vip, owners, expectedOwner)
}

func WaitForScaleLocalAdvertisement(ctx context.Context, clusterName string, nodes []string,
	client kubernetes.Interface, namespace, vip string, timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		readyNodes, err := scaleBackendReadyNodes(ctx, client, namespace)
		if err == nil && len(readyNodes) > 0 {
			presentNodes := make([]string, 0, len(nodes))
			for _, node := range nodes {
				if CheckIPAddressPresence(vip, node, true) {
					presentNodes = append(presentNodes, node)
				}
			}

			unexpected := ""
			for _, node := range presentNodes {
				if _, eligible := readyNodes[node]; !eligible {
					unexpected = node
					break
				}
			}
			if unexpected == "" && len(presentNodes) == 1 {
				return nil
			}
			if unexpected != "" {
				lastError = fmt.Errorf("VIP %q is advertised on node %q without a local ready endpoint", vip, unexpected)
			} else {
				lastError = fmt.Errorf("VIP %q is advertised on %d eligible nodes, want one", vip, len(presentNodes))
			}
		} else if err != nil {
			lastError = err
		} else {
			lastError = fmt.Errorf("no ready backend node for VIP %q", vip)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("local advertisement did not converge: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return err
		}
	}
}

func waitForScalePoll(ctx context.Context, deadline time.Time) error {
	poll := time.NewTimer(scalePollInterval)
	defer poll.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-poll.C:
		if time.Now().After(deadline) {
			return nil
		}
		return nil
	}
}

func SnapshotScaleMetrics(ctx context.Context, clusterName string, nodes []string) (ScaleMetricSnapshot, error) {
	snapshot := make(ScaleMetricSnapshot, len(nodes))
	for _, node := range nodes {
		metrics, err := ScrapeMetrics(ctx, clusterName, node)
		if err != nil {
			return nil, fmt.Errorf("scrape metrics from node %q: %w", node, err)
		}
		snapshot[node] = metrics
	}
	return snapshot, nil
}

func ScaleCounterDelta(before, after ScaleMetricSnapshot, name string, labelsForMetric map[string]string) (float64, error) {
	var total float64
	nodes := make(map[string]struct{}, len(before)+len(after))
	for node := range before {
		nodes[node] = struct{}{}
	}
	for node := range after {
		nodes[node] = struct{}{}
	}

	for node := range nodes {
		beforeMetrics := before[node]
		afterMetrics := after[node]
		beforeFound := len(matchingMetricValues(beforeMetrics, name, labelsForMetric)) > 0
		afterFound := len(matchingMetricValues(afterMetrics, name, labelsForMetric)) > 0
		if !beforeFound && !afterFound {
			continue
		}
		if !beforeFound && afterFound && SumMetric(afterMetrics, name, labelsForMetric) == 0 {
			// Prometheus counter vectors create a series lazily on first use.
			continue
		}
		if beforeFound != afterFound {
			return 0, fmt.Errorf("counter %q presence changed on node %q: before=%t after=%t", name, node, beforeFound, afterFound)
		}
		beforeValue := SumMetric(beforeMetrics, name, labelsForMetric)
		afterValue := SumMetric(afterMetrics, name, labelsForMetric)
		if afterValue < beforeValue {
			return 0, fmt.Errorf("counter %q reset on node %q: before=%v after=%v", name, node, beforeValue, afterValue)
		}
		total += afterValue - beforeValue
	}
	return total, nil
}

func ScaleTransitionDelta(before, after ScaleMetricSnapshot, leaseNames []string) (float64, bool) {
	var total float64
	found := false
	for node, afterMetrics := range after {
		beforeMetrics := before[node]
		for _, leaseName := range leaseNames {
			labelsForMetric := map[string]string{"lease_name": leaseName}
			if _, beforeMatches := MetricValue(beforeMetrics, "kube_vip_leader_election_transitions_total", labelsForMetric); beforeMatches == 0 {
				continue
			}
			if _, afterMatches := MetricValue(afterMetrics, "kube_vip_leader_election_transitions_total", labelsForMetric); afterMatches == 0 {
				continue
			}
			found = true
			beforeValue := SumMetric(beforeMetrics, "kube_vip_leader_election_transitions_total", labelsForMetric)
			afterValue := SumMetric(afterMetrics, "kube_vip_leader_election_transitions_total", labelsForMetric)
			delta := afterValue - beforeValue
			if delta < 0 {
				delta = afterValue
			}
			total += delta
		}
	}
	return total, found
}

func ScaleLeaseHolders(ctx context.Context, client kubernetes.Interface, namespace string, leaseNames []string) (map[string]string, error) {
	holders := make(map[string]string, len(leaseNames))
	for _, leaseName := range leaseNames {
		lease, err := client.CoordinationV1().Leases(namespace).Get(ctx, leaseName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get service lease %s/%s: %w", namespace, leaseName, err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return nil, fmt.Errorf("service lease %s/%s has no holder", namespace, leaseName)
		}
		holders[leaseName] = *lease.Spec.HolderIdentity
	}
	return holders, nil
}

func WaitForScaleLeaseTransfer(ctx context.Context, client kubernetes.Interface, namespace, leaseName, previousHolder string,
	eligibleNodes []string, timeout time.Duration,
) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		holders, err := ScaleLeaseHolders(ctx, client, namespace, []string{leaseName})
		if err == nil {
			if err := validateScaleLeaseTransfer(leaseName, previousHolder, holders[leaseName], eligibleNodes); err == nil {
				return holders[leaseName], nil
			} else {
				lastError = err
			}
		} else {
			lastError = err
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("service lease did not transfer: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return "", err
		}
	}
}

func validateScaleLeaseTransfer(leaseName, previousHolder, currentHolder string, eligibleNodes []string) error {
	if currentHolder == previousHolder {
		return fmt.Errorf("service lease %q is still held by suppressed node %q", leaseName, previousHolder)
	}
	for _, node := range eligibleNodes {
		if currentHolder == node {
			return nil
		}
	}
	return fmt.Errorf("service lease %q transferred to ineligible holder %q; eligible nodes are %v", leaseName, currentHolder, eligibleNodes)
}

func WaitForScaleLeaseHolder(ctx context.Context, client kubernetes.Interface, namespace, leaseName, expectedHolder string,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		holders, err := ScaleLeaseHolders(ctx, client, namespace, []string{leaseName})
		if err == nil && holders[leaseName] == expectedHolder {
			return nil
		}
		if err != nil {
			lastError = err
		} else {
			lastError = fmt.Errorf("service lease %s/%s is held by %q, want %q", namespace, leaseName, holders[leaseName], expectedHolder)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("service lease holder did not stabilize: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return err
		}
	}
}
