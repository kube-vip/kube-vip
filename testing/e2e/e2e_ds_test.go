//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	kindconfigv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/testing/e2e"
)

var _ = Describe("kube-vip ARP/NDP broadcast neighbor when deployed as a regular pod", Ordered, func() {
	if Mode == ModeARP {
		var (
			logger          log.Logger
			imagePath       string
			k8sImagePath    string
			configPath      string
			tempDirPathRoot string
		)

		ctx, cancel := context.WithCancel(context.TODO())

		BeforeEach(func() {
			klog.SetOutput(GinkgoWriter)
			logger = e2e.TestLogger{}

			imagePath = os.Getenv("E2E_IMAGE_PATH")    // Path to kube-vip image
			configPath = os.Getenv("CONFIG_PATH")      // path to the api server config
			k8sImagePath = os.Getenv("K8S_IMAGE_PATH") // path to the kubernetes image (version for kind)
			if configPath == "" {
				configPath = "/etc/kubernetes/admin.conf"
			}
		})

		BeforeAll(func() {
			var err error
			tempDirPathRoot, err = os.MkdirTemp("", "kube-vip-test-arp")
			Expect(err).NotTo(HaveOccurred())

		})

		AfterAll(func() {
			if os.Getenv("E2E_KEEP_LOGS") != "true" {
				Expect(os.RemoveAll(tempDirPathRoot)).To(Succeed())
			}
			cancel()
		})

		Describe("kube-vip IPv4 functionality, vip_leaderelection=true", Ordered, func() {
			var (
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv4Family,
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareClusterForDS(ctx, tempDirPath, "ipv4-ds", imagePath, k8sImagePath,
					logger, networking, 1, nil, 1)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(ctx, client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" deletes all IPv4 addresses on exit when only control plane is enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" deletes all IPv4 addresses on exit when only services are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" deletes all IPv4 addresses on exit when only services with per-service leaderelection are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" deletes all IPv4 addresses on exit when control-plane and services are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})

			It(clusterName+" deletes all IPv4 addresses on exit when ontrol plane and services with per-service leaderelection are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv4Family, clusterName)
			})
		})

		Describe("kube-vip IPv6 functionality, vip_leaderelection=true", Ordered, func() {
			var (
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.IPv6Family,
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareClusterForDS(ctx, tempDirPath, "ipv6-ds", imagePath, k8sImagePath,
					logger, networking, 1, nil, 1)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(ctx, client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" deletes all IPv6 addresses on exit when only control plane is enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv6Family, clusterName)
			})

			It(clusterName+" deletes all IPv6 addresses on exit when only services are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv6Family, clusterName)
			})

			It(clusterName+" deletes all IPv6 addresses on exit when only services with per-service leaderelection are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv6Family, clusterName)
			})

			It(clusterName+" deletes all IPv6 addresses on exit when control-plane and services are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv6Family, clusterName)
			})

			It(clusterName+" deletes all IPv6 addresses on exit when ontrol plane and services with per-service leaderelection are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.IPv6Family, clusterName)
			})
		})

		Describe("kube-vip DualStack functionality, vip_leaderelection=true", Ordered, func() {
			var (
				client      kubernetes.Interface
				clusterName string
				tempDirPath string
			)

			BeforeAll(func() {
				networking := &kindconfigv1alpha4.Networking{
					IPFamily: kindconfigv1alpha4.DualStackFamily,
				}

				var err error
				tempDirPath, err = os.MkdirTemp(tempDirPathRoot, "kube-vip-test")
				Expect(err).NotTo(HaveOccurred())

				clusterName, client, _ = prepareClusterForDS(ctx, tempDirPath, "ipdual-ds", imagePath, k8sImagePath,
					logger, networking, 1, nil, 1)
			})

			AfterAll(func() {
				Eventually(func() error {
					return e2e.GetLogs(ctx, client, tempDirPath)
				}, "60s", "5s").Should(Succeed())
				cleanupCluster(clusterName, ConfigMtx, logger)
			})

			It(clusterName+" deletes all addresses on exit when only control plane is enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "false",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.DualFamily, clusterName)
			})

			It(clusterName+" deletes all addresses on exit when only services are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.DualFamily, clusterName)
			})

			It(clusterName+" deletes all addresses on exit when only services with per-service leaderelection are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "false",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.DualFamily, clusterName)
			})

			It(clusterName+" deletes all addresses on exit when control-plane and services are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "false",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.DualFamily, clusterName)
			})

			It(clusterName+" deletes all addresses on exit when ontrol plane and services with per-service leaderelection are enabled", func() {
				manifestValues := &e2e.KubevipManifestValues{
					Mode:                 Mode,
					ControlPlaneEnable:   "true",
					VipElectionEnable:    "true",
					ImagePath:            imagePath,
					ConfigPath:           configPath,
					SvcEnable:            "true",
					SvcElectionEnable:    "true",
					EnableEndpointslices: "true",
					EnableNodeLabeling:   "false",
				}

				testDsArp(ctx, manifestValues, client, utils.DualFamily, clusterName)
			})
		})
	}
})

func testDsArp(ctx context.Context, manifestValues *e2e.KubevipManifestValues, client kubernetes.Interface, family, clusterName string) {

	cpEnable, err := strconv.ParseBool(manifestValues.ControlPlaneEnable)
	Expect(err).ToNot(HaveOccurred())

	svcEnable, err := strconv.ParseBool(manifestValues.SvcEnable)
	Expect(err).ToNot(HaveOccurred())

	vipElection, err := strconv.ParseBool(manifestValues.VipElectionEnable)
	Expect(err).ToNot(HaveOccurred())

	svcElection, err := strconv.ParseBool(manifestValues.SvcElectionEnable)
	Expect(err).ToNot(HaveOccurred())

	vipUseLease, svcUseLease, svcPerServiceElection := false, false, false

	switch manifestValues.Mode {
	case ModeARP:
		vipUseLease = true
		svcUseLease = true
		svcPerServiceElection = svcElection
	case ModeRT:
		if vipElection || svcElection {
			svcUseLease = true
		}
		if svcElection {
			svcPerServiceElection = true
		}
	case ModeBGP:
		if vipElection {
			vipUseLease = true
		}
		if svcElection {
			svcUseLease = true
			svcPerServiceElection = true
		}
	}

	genFam := family
	if family == utils.DualFamily {
		genFam = e2e.DualstackFamily
	}

	if cpEnable {
		manifestValues.ControlPlaneVIP = e2e.GenerateVIP(genFam, SOffset.Get())
	}

	createKubeVipDS(ctx, "kube-vip", "kube-system", manifestValues, client)

	families := []corev1.IPFamily{}
	switch family {
	case utils.IPv4Family:
		families = []corev1.IPFamily{corev1.IPv4Protocol}
	case utils.IPv6Family:
		families = []corev1.IPFamily{corev1.IPv6Protocol}
	case utils.DualFamily:
		families = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}
	}

	var cpVips, svcVips, services []string
	var cpHost, svcHost, svcVip string

	if cpEnable {
		if vipUseLease {
			cpHost = e2e.GetLeaseHolder(ctx, "plndr-cp-lock", "kube-system", client)
			By(withTimestamp("control plane lease holder: " + cpHost))
		} else {
			cpHost = fmt.Sprintf("%s-control-plane", clusterName)
		}

		cpVips = strings.Split(manifestValues.ControlPlaneVIP, ",")
		for _, addr := range cpVips {
			By(withTimestamp("checking control plane address was added: " + addr))
			Expect(checkIPAddress(addr, cpHost, true)).To(BeTrue())
		}
	}

	if svcEnable {
		leaseName := "plndr-svcs-lock"
		leaseNamespace := "kube-system"

		svcVip = e2e.GenerateVIP(genFam, SOffset.Get())
		var leases []string
		services, leases = createTestServiceForDS(ctx, "test-svc", svcVip, leaseName,
			corev1.ServiceExternalTrafficPolicyCluster, client, svcElection, families, 1)

		if svcUseLease {
			if svcPerServiceElection {
				Expect(leases).ToNot(BeEmpty())
				leaseName = leases[0]
				leaseNamespace = dsNamespace
			}
			svcHost = e2e.GetLeaseHolder(ctx, leaseName, leaseNamespace, client)
			By(withTimestamp(fmt.Sprintf("services lease %s/%s holder: %s", leaseNamespace, leaseName, svcHost)))
		} else {
			svcHost = fmt.Sprintf("%s-control-plane", clusterName)
		}

		svcVips = strings.Split(svcVip, ",")

		for _, addr := range svcVips {
			By(withTimestamp("checking address was added: " + addr))
			Expect(checkIPAddress(addr, svcHost, true)).To(BeTrue())
		}
	}

	pods, err := client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "app=kube-vip"})
	Expect(err).ToNot(HaveOccurred())
	Expect(len(pods.Items)).To(Equal(1))

	removeKubeVipDS(ctx, client)

	// check if kube-vip pod gets deleted within 10s
	Eventually(func() error {
		_, err := client.CoreV1().Pods("kube-system").Get(ctx, pods.Items[0].Name, metav1.GetOptions{})
		return err
	}, "10s", "200ms").ShouldNot(Succeed())

	if cpEnable && manifestValues.Mode == ModeARP {
		for _, addr := range cpVips {
			By(withTimestamp("checking CP address was removed: " + addr))
			Expect(checkIPAddress(addr, cpHost, false)).To(BeTrue())
		}
	}

	if svcEnable {
		if manifestValues.Mode == ModeARP {
			for _, addr := range svcVips {
				By(withTimestamp("checking service address was removed: " + addr))
				Expect(checkIPAddress(addr, svcHost, false)).To(BeTrue())
			}
		}

		for _, svc := range services {
			By(withTimestamp("deleting service: " + svc))
			Expect(client.CoreV1().Services(dsNamespace).Delete(ctx, svc, metav1.DeleteOptions{})).To(Succeed())
			time.Sleep(time.Second)
		}
	}
}

func createKubeVipDS(ctx context.Context, name, namespace string, manifestValues *e2e.KubevipManifestValues, client kubernetes.Interface) {
	labels := make(map[string]string)
	labels["app"] = name
	d := v1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
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
					},
					Containers: []corev1.Container{
						{
							Name:            name,
							Args:            []string{"manager", "--prometheusHTTPServer", ""},
							Image:           manifestValues.ImagePath,
							ImagePullPolicy: corev1.PullNever,
							Env: []corev1.EnvVar{
								{
									Name:  "vip_loglevel",
									Value: "-4",
								},
								{
									Name:  "vip_interface",
									Value: "eth0",
								},
								{
									Name:  "vip_leaderelection",
									Value: manifestValues.VipElectionEnable,
								},
								{
									Name:  "address",
									Value: manifestValues.ControlPlaneVIP,
								},
								{
									Name:  "vip_leaseduration",
									Value: "5",
								},
								{
									Name:  "vip_renewdeadline",
									Value: "3",
								},
								{
									Name:  "vip_retryperiod",
									Value: "1",
								},
								{
									Name:  "cp_enable",
									Value: manifestValues.ControlPlaneEnable,
								},
								{
									Name: "vip_nodename",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name:  "svc_enable",
									Value: manifestValues.SvcEnable,
								},
								{
									Name:  "svc_election",
									Value: manifestValues.SvcElectionEnable,
								},
								{
									Name:  "enable_node_labeling",
									Value: manifestValues.EnableNodeLabeling,
								},
								{
									Name:  "enable_endpointslices",
									Value: manifestValues.EnableEndpointslices,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"NET_ADMIN",
										"NET_RAW",
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "kubeconfig",
									MountPath: "/etc/kubernetes/admin.conf",
								},
							},
						},
					},
					HostAliases: []corev1.HostAlias{
						{
							Hostnames: []string{"kubernetes"},
							IP:        "127.0.0.1",
						},
					},
					HostNetwork: true,
					Volumes: []corev1.Volume{
						{
							Name: "kubeconfig",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: manifestValues.ConfigPath,
								},
							},
						},
					},
				},
			},
		},
	}

	var modeEnv []corev1.EnvVar

	switch manifestValues.Mode {
	case "rt":
		modeEnv = append(modeEnv, corev1.EnvVar{
			Name:  "vip_routingtable",
			Value: "true",
		})
	case "bgp":
		modeEnv = append(modeEnv,
			corev1.EnvVar{
				Name:  "bgp_enable",
				Value: "true",
			},
			corev1.EnvVar{
				Name:  "bgp_routerid",
				Value: "2.2.2.2",
			},
			corev1.EnvVar{
				Name:  "bgp_as",
				Value: fmt.Sprintf("%d", manifestValues.BGPAS),
			},
			corev1.EnvVar{
				Name:  "bgp_peers",
				Value: manifestValues.BGPPeers,
			},
		)
	case "wireguard":
		modeEnv = append(modeEnv, corev1.EnvVar{
			Name:  "vip_wireguard",
			Value: "true",
		})
	default:
		modeEnv = append(modeEnv, corev1.EnvVar{
			Name:  "vip_arp",
			Value: "true",
		})
	}

	d.Spec.Template.Spec.Containers[0].Env = append(d.Spec.Template.Spec.Containers[0].Env, modeEnv...)

	By(withTimestamp("creating kube-vip's daemonset"))
	_, err := client.AppsV1().DaemonSets(namespace).Create(ctx, &d, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())

	Eventually(func() error {
		return dsStarted(ctx, name, namespace, client)
	}, "300s").Should(Succeed())
}

func removeKubeVipDS(ctx context.Context, client kubernetes.Interface) {
	By(withTimestamp("removing kube-vip's daemonset"))
	Eventually(func() error {
		return client.AppsV1().DaemonSets("kube-system").Delete(ctx, "kube-vip", metav1.DeleteOptions{})
	}, "300s", "200ms").Should(Succeed())
}

func prepareClusterForDS(ctx context.Context, tempDirPath, clusterNameSuffix, kvImagePath, k8sImagePath string, logger log.Logger,
	networking *kindconfigv1alpha4.Networking, nodesNum int,
	addSAN *san, dsNumber int) (string, kubernetes.Interface, *rest.Config) {

	clusterConfig := kindconfigv1alpha4.Cluster{
		Networking: *networking,
		Nodes:      []kindconfigv1alpha4.Node{},
	}

	kubeadmPatches := []kindconfigv1alpha4.PatchJSON6902{}

	if addSAN != nil {
		for range 64 {
			(*addSAN.ip)[len(*addSAN.ip)-1]++
			if addSAN.ipnet.Contains(*addSAN.ip) {
				kubeadmPatches = append(kubeadmPatches, kindconfigv1alpha4.PatchJSON6902{
					Group:   "kubeadm.k8s.io",
					Version: "v1beta3",
					Kind:    "ClusterConfiguration",
					Patch:   testJSON + addSAN.ip.String(),
				})
			}
		}
	}

	clusterConfig.KubeadmConfigPatchesJSON6902 = kubeadmPatches

	for range nodesNum {
		nodeConfig := kindconfigv1alpha4.Node{
			Role: kindconfigv1alpha4.ControlPlaneRole,
		}
		// Override the kind image version
		if k8sImagePath != "" {
			nodeConfig.Image = k8sImagePath
		}
		clusterConfig.Nodes = append(clusterConfig.Nodes, nodeConfig)
	}

	clusterName := fmt.Sprintf("%s-%s", filepath.Base(tempDirPath), clusterNameSuffix)

	By(withTimestamp("creating a kind cluster with multiple control plane nodes"))
	client, cfg, err := createKindCluster(logger, &clusterConfig, clusterName)
	Expect(err).ToNot(HaveOccurred())

	By(withTimestamp("loading local docker image to kind cluster"))
	Expect(e2e.LoadDockerImageToKind(logger, kvImagePath, clusterName)).To(Succeed())

	By(withTimestamp("loading traefik/whoami image to kind cluster"))
	if err := e2e.LoadDockerImageToKind(logger, "ghcr.io/traefik/whoami:v1.11", clusterName); err != nil {
		By(withTimestamp(fmt.Sprintf(
			"failed to load image ghcr.io/traefik/whoami:v1.11 into kind cluster, image will be downloaded after pod deployment, error: %s",
			err.Error(),
		)))
	}

	By(withTimestamp("creating test daemonset"))
	for i := range dsNumber {
		tmpDsName := dsName
		if i > 0 {
			tmpDsName = fmt.Sprintf("%s-%d", dsName, i)
		}
		createTestDS(ctx, tmpDsName, dsNamespace, client, 80+i)
	}

	return clusterName, client, cfg
}

func createTestServiceForDS(ctx context.Context, svcName, lbAddress, leaseName string, trafficPolicy corev1.ServiceExternalTrafficPolicy,
	client kubernetes.Interface, serviceElection bool, ipFamily []corev1.IPFamily, numberOfServices int) ([]string, []string) {
	services := []string{}
	leases := []string{}
	for i := range numberOfServices {
		services = append(services, fmt.Sprintf("%s-%d", svcName, i))
		if serviceElection {
			leases = append(leases, fmt.Sprintf("kubevip-%s-%d", svcName, i))
		} else {
			leases = append(leases, leaseName)
		}

	}

	for _, svc := range services {
		createTestService(ctx, svc, dsNamespace, dsName, lbAddress,
			client, corev1.IPFamilyPolicyPreferDualStack, ipFamily, trafficPolicy, "", 80)
		time.Sleep(time.Second)
	}

	return services, leases
}
