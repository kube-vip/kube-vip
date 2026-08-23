//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
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
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

const (
	scaleWorkerKubeconfigPath = "/etc/kubernetes/admin.conf"
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

func InstallScaleWorkerKubeconfigs(cluster *Cluster) error {
	if cluster == nil || cluster.Provider == nil {
		return fmt.Errorf("scale cluster or provider is nil")
	}

	kubeconfig, err := cluster.Provider.KubeConfig(cluster.Name, false)
	if err != nil {
		return fmt.Errorf("get internal kubeconfig for cluster %q: %w", cluster.Name, err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(kubeconfig))

	for _, node := range cluster.Nodes {
		role, err := node.Role()
		if err != nil {
			return fmt.Errorf("get role for node %q: %w", node.String(), err)
		}
		if role != string(kindconfigv1alpha4.WorkerRole) {
			continue
		}

		script := fmt.Sprintf(
			"printf '%%s' '%s' | base64 -d > %s && chmod 600 %s",
			encoded,
			scaleWorkerKubeconfigPath,
			scaleWorkerKubeconfigPath,
		)
		if err := node.Command("bash", "-c", script).Run(); err != nil {
			return fmt.Errorf("install kubeconfig on worker %q: %w", node.String(), err)
		}
	}

	return nil
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
) error {
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
	_, err := client.CoreV1().Services(namespace).Create(ctx, &corev1.Service{
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
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create service %s/%s: %w", namespace, name, err)
	}
	return nil
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

func WaitForScaleMetrics(clusterName string, nodes []string) error {
	for _, node := range nodes {
		if _, err := ScrapeMetrics(clusterName, node); err != nil {
			return fmt.Errorf("scrape metrics from node %q: %w", node, err)
		}
	}
	return nil
}

func ScaleActiveServices(clusterName string, nodes []string, namespace string) (float64, error) {
	maximum := 0.0
	for _, node := range nodes {
		metrics, err := ScrapeMetrics(clusterName, node)
		if err != nil {
			return 0, err
		}
		value, matches := MetricValue(metrics, "kube_vip_active_services", map[string]string{"namespace": namespace})
		if matches > 1 {
			return 0, fmt.Errorf("active-services metric on node %q matched %d series", node, matches)
		}
		if matches == 1 && value > maximum {
			maximum = value
		}
	}
	return maximum, nil
}

func WaitForScaleActiveServices(ctx context.Context, clusterName string, nodes []string, namespace string, expected int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		active, err := ScaleActiveServices(clusterName, nodes, namespace)
		if err == nil {
			if active == float64(expected) {
				return nil
			}
			lastError = fmt.Errorf("active service metric is %.0f, want %d", active, expected)
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

func WaitForScaleVIPs(clusterName string, nodes []string, vips []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		allAdvertised := true
		for _, vip := range vips {
			advertised := false
			for _, node := range nodes {
				if CheckIPAddressPresence(vip, node, true) {
					advertised = true
					break
				}
			}
			if !advertised {
				allAdvertised = false
				lastError = fmt.Errorf("VIP %q was not found on any Kind node", vip)
				break
			}
		}
		if allAdvertised {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("VIP spot checks did not converge: %w", lastError)
		}
		time.Sleep(scalePollInterval)
	}
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

func SnapshotScaleMetrics(clusterName string, nodes []string) (ScaleMetricSnapshot, error) {
	snapshot := make(ScaleMetricSnapshot, len(nodes))
	for _, node := range nodes {
		metrics, err := ScrapeMetrics(clusterName, node)
		if err != nil {
			return nil, fmt.Errorf("scrape metrics from node %q: %w", node, err)
		}
		snapshot[node] = metrics
	}
	return snapshot, nil
}

func ScaleCounterDelta(before, after ScaleMetricSnapshot, name string, labelsForMetric map[string]string) float64 {
	var total float64
	for node, afterMetrics := range after {
		beforeMetrics := before[node]
		beforeValue := SumMetric(beforeMetrics, name, labelsForMetric)
		afterValue := SumMetric(afterMetrics, name, labelsForMetric)
		delta := afterValue - beforeValue
		if delta < 0 {
			delta = afterValue
		}
		total += delta
	}
	return total
}

func ScaleTransitionDelta(before, after ScaleMetricSnapshot, leaseNames []string) float64 {
	var total float64
	for node, afterMetrics := range after {
		beforeMetrics := before[node]
		for _, leaseName := range leaseNames {
			labelsForMetric := map[string]string{"lease_name": leaseName}
			beforeValue := SumMetric(beforeMetrics, "kube_vip_leader_election_transitions_total", labelsForMetric)
			afterValue := SumMetric(afterMetrics, "kube_vip_leader_election_transitions_total", labelsForMetric)
			delta := afterValue - beforeValue
			if delta < 0 {
				delta = afterValue
			}
			total += delta
		}
	}
	return total
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

func WaitForScaleLeaseHolders(ctx context.Context, client kubernetes.Interface, namespace string,
	leaseNames []string, previous map[string]string, killedNode string, timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastError error
	for {
		holders, err := ScaleLeaseHolders(ctx, client, namespace, leaseNames)
		if err == nil {
			allReady := true
			for _, leaseName := range leaseNames {
				holder := holders[leaseName]
				if previous[leaseName] == killedNode && holder == killedNode {
					allReady = false
					lastError = fmt.Errorf("service lease %s/%s is still held by killed node %q", namespace, leaseName, killedNode)
					break
				}
			}
			if allReady {
				return nil
			}
		} else {
			lastError = err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("service leases did not reacquire: %w", lastError)
		}
		if err := waitForScalePoll(ctx, deadline); err != nil {
			return err
		}
	}
}
