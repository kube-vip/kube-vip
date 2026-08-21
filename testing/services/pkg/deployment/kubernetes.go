package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/gookit/slog"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
)

// service defines the settings for a new service
type Service struct {
	name           string
	namespace      string
	egress         bool // enable egress
	egressInternal bool // enable Internal egress
	egressIPv6     bool // egress should be IPv6
	policyLocal    bool // set the policy to local pods
	testHTTP       bool
	testDualstack  bool   // test dualstack loadbalancer services
	dhcpBroadcast  bool   // test dhcp broadcast flag
	commonLease    string // share a lease with other services under this name
	timeout        int    // how long to wait for the service to be created
}

func (s *Service) ns() string {
	if s.namespace != "" {
		return s.namespace
	}
	return v1.NamespaceDefault
}

type Deployment struct {
	replicas     int32
	server       bool
	client       bool
	address      string
	nodeAffinity string
	name         string
	namespace    string
}

func backendLabels(instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "kube-vip-e2e",
		"app.kubernetes.io/instance": instance,
	}
}

func (d *Deployment) ns() string {
	if d.namespace != "" {
		return d.namespace
	}
	return v1.NamespaceDefault
}

// buildKVDsDaemonSet returns the kube-vip DaemonSet spec for ns.
// metricsAddr is passed as the --prometheusHTTPServer container arg; pass empty
// string to disable prometheus on parallel-test nodes where multiple DaemonSets
// share the same host network. It has to be an arg: the env var equivalent
// (prometheus_server) is ignored by ParseEnvironment when empty, so an env var
// cannot override the :2112 flag default.
func buildKVDsDaemonSet(ns, imageURL, metricsAddr string, globalWatch bool) appsv1.DaemonSet {
	labels := map[string]string{
		"app":                        "kube-vip",
		"app.kubernetes.io/name":     "kube-vip-ds",
		"app.kubernetes.io/instance": ns,
	}
	env := []v1.EnvVar{
		{Name: "vip_arp", Value: "true"},
		{Name: "vip_subnet", Value: "auto,auto"},
		{Name: "svc_enable", Value: "true"},
		{Name: "egress_podcidr", Value: "10.244.0.0/16,fd00:10:244::/56"},
		{Name: "enable_endpoints", Value: "false"},
		{Name: "svc_election", Value: "true"},
		{Name: "EGRESS_CLEAN", Value: "true"},
		{Name: "vip_loglevel", Value: "-4"},
		{Name: "egress_withnftables", Value: "true"},
		// instance_name scopes the nftables egress table to this namespace so
		// parallel DaemonSets on the same host network don't overwrite each other.
		{Name: "instance_name", Value: ns},
		{Name: "vip_nodename", ValueFrom: &v1.EnvVarSource{
			FieldRef: &v1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
	}
	if !globalWatch {
		env = append(env, v1.EnvVar{Name: "svc_namespace", Value: ns})
	}
	var gracePeriod int64 = 10
	return appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-ds", Namespace: ns, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name":     "kube-vip-ds",
				"app.kubernetes.io/instance": ns,
			}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: v1.PodSpec{
					TerminationGracePeriodSeconds: &gracePeriod,
					ServiceAccountName:            "kube-vip",
					HostNetwork:                   true,
					Containers: []v1.Container{{
						Name:  "kube-vip",
						Image: imageURL,
						Args:  []string{"manager", "--prometheusHTTPServer=" + metricsAddr},
						Env:   env,
						SecurityContext: &v1.SecurityContext{
							Capabilities: &v1.Capabilities{Add: []v1.Capability{"NET_ADMIN", "NET_RAW"}},
						},
						ImagePullPolicy: v1.PullIfNotPresent,
					}},
				},
			},
		},
	}
}

// CreateNamespacedKVDs creates a kube-vip DaemonSet and all RBAC resources
// inside ns. The DaemonSet is scoped to watch only ns so no ClusterRole for
// services or leases is needed; only nodes remain cluster-scoped.
func (d *Deployment) CreateNamespacedKVDs(ctx context.Context, clientset *kubernetes.Clientset, imageURL, ns string, globalWatch bool, metricsAddr string) error {
	// ServiceAccount
	sa := &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "kube-vip", Namespace: ns}}
	if _, err := clientset.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("serviceaccount: %w", err)
	}

	// Role or ClusterRole for service/endpoint/lease access
	if globalWatch {
		// bind the shared kube-vip-services ClusterRole so this SA can watch across all namespaces
		serviceCRB := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-services-" + ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kube-vip-services"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kube-vip", Namespace: ns}},
		}
		if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, serviceCRB, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("clusterrolebinding (services): %w", err)
		}
	} else {
		// per-namespace Role limits kube-vip to this namespace only
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-vip", Namespace: ns},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"services", "services/status", "endpoints"}, Verbs: []string{"list", "get", "watch", "update"}},
				{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"list", "get", "watch", "update", "create"}},
				{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "get", "watch"}},
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
				{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create"}},
			},
		}
		if _, err := clientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("role: %w", err)
		}

		// RoleBinding
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-vip", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "kube-vip"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kube-vip", Namespace: ns}},
		}
		if _, err := clientset.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("rolebinding: %w", err)
		}
	}

	// ClusterRoleBinding giving this namespace's SA access to nodes (cluster-scoped)
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-nodes-" + ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kube-vip-nodes"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kube-vip", Namespace: ns}},
	}
	if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("clusterrolebinding: %w", err)
	}

	// DaemonSet scoped to this namespace
	ds := buildKVDsDaemonSet(ns, imageURL, metricsAddr, globalWatch)
	if _, err := clientset.AppsV1().DaemonSets(ns).Create(ctx, &ds, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("daemonset: %w", err)
	}
	return nil
}
func (d *Deployment) CreateDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	replicas := d.replicas
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: d.name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: backendLabels(d.ns()),
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: backendLabels(d.ns()),
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "kube-vip-web",
							Image: "docker.io/plndr/e2e:0.0.1",
							Ports: []v1.ContainerPort{
								{
									Name:          "http",
									Protocol:      v1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
							ImagePullPolicy: v1.PullIfNotPresent,
						},
					},
				},
			},
		},
	}

	if d.server {
		deployment.Spec.Template.Spec.Containers[0].Env =
			[]v1.EnvVar{
				{
					Name:  "E2EMODE",
					Value: "SERVER",
				},
			}
	}

	if d.client && d.address != "" {
		deployment.Spec.Template.Spec.Containers[0].Env =
			[]v1.EnvVar{
				{
					Name:  "E2EMODE",
					Value: "CLIENT",
				},
				{
					Name:  "E2EADDRESS",
					Value: d.address,
				},
			}
	}

	if d.nodeAffinity != "" {
		deployment.Spec.Template.Spec.NodeName = d.nodeAffinity
	}

	result, err := clientset.AppsV1().Deployments(d.ns()).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	slog.Infof("📝 created deployment [%s]", result.GetObjectMeta().GetName())
	return nil
}

// WaitForAvailable blocks until at least one replica of the deployment is ready,
// or ctx is cancelled. It polls every 2 s with a hard ceiling of 60 s.
func (d *Deployment) WaitForAvailable(ctx context.Context, clientset *kubernetes.Clientset) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		dep, err := clientset.AppsV1().Deployments(d.ns()).Get(ctx, d.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get deployment %s: %w", d.name, err)
		}
		if dep.Status.ReadyReplicas >= 1 {
			slog.Infof("📝 deployment [%s] has %d ready replica(s)", d.name, dep.Status.ReadyReplicas)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s not ready after 60 s", d.name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Service) CreateService(ctx context.Context, clientset *kubernetes.Clientset) (currentLeader string, loadBalancerAddresses []string, err error) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.name,
			Namespace: s.ns(),
			Labels:    backendLabels(s.ns()),
		},

		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Port:     80,
					Protocol: v1.ProtocolTCP,
				},
			},
			Selector:  backendLabels(s.ns()),
			ClusterIP: "",
			Type:      v1.ServiceTypeLoadBalancer,
		},
	}

	svc.Annotations = map[string]string{}

	if s.egress {
		svc.Annotations[kubevip.Egress] = "true"
	}

	if s.commonLease != "" {
		// A common lease is only valid with a cluster traffic policy.
		svc.Annotations[kubevip.ServiceLease] = s.commonLease
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	}

	if s.egressInternal {
		svc.Annotations[kubevip.EgressInternal] = "true"
	}

	if s.egressIPv6 {
		svc.Annotations[kubevip.EgressIPv6] = "true"
	}

	//svc.Annotations[kubevip.EgressDeniedNetworks] = "172.18.0.0/24"
	//svc.Annotations[kubevip.EgressAllowedNetworks] = "172.18.0.0/24"

	//svc.Annotations[kubevip.EgressAllowedNetworks] = "192.168.0.0/24, 172.18.0.0/24"

	if s.policyLocal {
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	}

	if s.dhcpBroadcast {
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		svc.Annotations[kubevip.DHCPBroadcast] = "true"
	}

	if s.testDualstack {
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		ipv4VIP, err := generateIPv4VIP()
		if err != nil {
			slog.Fatal(err)
		}
		ipv6VIP, err := generateIPv6VIP()
		if err != nil {
			slog.Fatal(err)
		}
		svc.Annotations[kubevip.LoadbalancerIPAnnotation] = fmt.Sprintf("%s,%s", ipv4VIP, ipv6VIP)
		svc.Labels["implementation"] = "kube-vip"
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
		ipfPolicy := v1.IPFamilyPolicyRequireDualStack
		svc.Spec.IPFamilyPolicy = &ipfPolicy
	}

	slog.Infof("🌍 creating service [%s]", svc.Name)
	_, err = clientset.CoreV1().Services(s.ns()).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		slog.Fatal(err)
	}
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	rw, err := watchtools.NewRetryWatcherWithContext(ctx, "1", &cache.ListWatch{
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Services(s.ns()).Watch(ctx, metav1.ListOptions{})
		},
	})
	if err != nil {
		slog.Fatal(err)
	}
	ch := rw.ResultChan()
	go func() {
		time.Sleep(time.Second * time.Duration(s.timeout))
		rw.Stop()
	}()
	ready := false

	// Used for tracking an active endpoint / pod
	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			svc, ok := event.Object.(*v1.Service)
			if !ok {
				slog.Fatalf("unable to parse Kubernetes services from API watcher")
			}
			if svc.Name == s.name {
				if len(svc.Status.LoadBalancer.Ingress) != 0 {
					for _, ingress := range svc.Status.LoadBalancer.Ingress {
						loadBalancerAddresses = append(loadBalancerAddresses, ingress.IP)
					}
					slog.Infof("🔎 found load balancer addresses [%s] on node [%s]", loadBalancerAddresses, svc.Annotations[kubevip.VipHost])
					ready = true
					currentLeader = svc.Annotations[kubevip.VipHost]
				}
			}
		default:

		}
		if ready {
			break
		}
	}
	if s.testHTTP {
		for _, lbAddress := range loadBalancerAddresses {
			err = httpTest(lbAddress)
			if err != nil {
				slog.Infof("web retrieval err: %s", err.Error())
				return "", nil, fmt.Errorf("web retrieval timeout ")

			}
		}
	}
	return currentLeader, loadBalancerAddresses, nil
}
