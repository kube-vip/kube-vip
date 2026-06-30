package cmd

import (
	"fmt"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/spf13/cobra"
)

// manifests will eventually deprecate the kubeadm set of subcommands
// manifests will be used to generate:
// - Pod spec manifest, mainly used for a static pod (kubeadm)
// - Daemonset manifest, mainly used to run kube-vip as a deamonset within Kubernetes (k3s/rke)
// - RBAC manifest, used to generate the RBAC permissions for kube-vip

var taint, role, rolebinding bool

func init() {
	kubeManifest.PersistentFlags().BoolVar(&inCluster, "inCluster", false, "Use the incluster token to authenticate to Kubernetes")
	kubeManifest.PersistentFlags().StringVar(&image, "image", "ghcr.io/kube-vip/kube-vip", "Define a hardcoded image with or without tag for the manifest")
	kubeManifestDaemon.PersistentFlags().BoolVar(&taint, "taint", false, "Taint the manifest for only running on control planes")
	kubeManifestRbac.PersistentFlags().BoolVar(&role, "role", false, "Generate only a Role inside the serviceNamespace access")
	kubeManifestRbac.PersistentFlags().BoolVar(&rolebinding, "rolebinding", false, "Generate only a RoleBinding for namespaced access")

	kubeManifest.AddCommand(kubeManifestPod)
	kubeManifest.AddCommand(kubeManifestDaemon)
	kubeManifest.AddCommand(kubeManifestRbac)
}

var kubeManifest = &cobra.Command{
	Use:   "manifest",
	Short: "Manifest functions",
	Long: `This command group provides flexible manifest generation for deploying kube-vip in various Kubernetes environments.

Unlike the 'kubeadm' subcommands, which are tightly coupled to kubeadm's static Pod requirements, these generators produce standard Kubernetes manifests (Pod, DaemonSet, RBAC) that can be used with any Kubernetes distribution (e.g., k3s, RKE, or vanilla Kubernetes).

Subcommands:
  pod        : Generates a standalone Pod manifest (similar to a static pod).
  daemonset  : Generates a DaemonSet manifest to run kube-vip on selected nodes.
  rbac       : Generates the necessary ServiceAccount, Role/ClusterRole, and Binding manifests.

All output is written to stdout as YAML, typically piped to 'kubectl apply -f -' or saved to a file.`,
	Run: func(cmd *cobra.Command, _ []string) {
		_ = cmd.Help()
	},
}

var kubeManifestPod = &cobra.Command{
	Use:   "pod",
	Short: "Generate a Pod Manifest",
	Long: `Generate a standalone Pod manifest for kube-vip.

This is ideal for environments that do not use DaemonSets or where you want to run kube-vip as a static Pod (similar to the 'kubeadm' subcommand, but without kubeadm-specific assumptions). It includes all the necessary container specifications, volumes, and environment variables derived from the provided flags.

Key flags:
  --interface : Network interface for the VIP.
  --vip or --address : The Virtual IP address or DNS name.
  --image     : Override the container image (default: ghcr.io/kube-vip/kube-vip).

The manifest is generated based on the current configuration flags set on the root command.

Example:
  kube-vip manifest pod --interface eth0 --vip 10.0.0.100 --controlplane | kubectl apply -f -`,
	Run: func(cmd *cobra.Command, _ []string) {
		var err error

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Error("parsing environment", "err", err)
			return
		}
		if err := initConfig.Validate(); err != nil {
			log.Error("validating configuration", "err", err)
			return
		}

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Error("no address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address, initConfig.DNSMode)
			if err != nil {
				log.Error("config parse", "err", err)
				return
			}
		}

		cfg, err := kubevip.GeneratePodManifestFromConfig(&initConfig, image, Release.Version, inCluster)
		if err != nil {
			log.Error("unable to create manifest", "err", err)
			return
		}
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeManifestDaemon = &cobra.Command{
	Use:   "daemonset",
	Short: "Generate a Daemonset Manifest",
	Long: `Generate a DaemonSet manifest to run kube-vip across multiple nodes.

This is the recommended deployment method for production clusters running kube-vip as a service. It ensures that kube-vip runs on all control-plane nodes (or selected nodes via tolerations) and can handle both control-plane HA and service load-balancing.

Flags specific to this subcommand:
  --taint  : Adds a toleration to the DaemonSet so that pods are scheduled only on nodes with the control-plane taint (node-role.kubernetes.io/control-plane:NoSchedule). This is essential for control-plane-only deployments.

All other standard kube-vip flags (--interface, --vip, --enableARP, --enableBGP, etc.) are respected and embedded into the DaemonSet pod template.

Example:
  kube-vip manifest daemonset --interface eth0 --vip 192.168.1.100 --controlplane --taint | kubectl apply -f -`,
	Run: func(cmd *cobra.Command, _ []string) {
		var err error

		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Error("parsing environment", "err", err)
			return
		}
		if err := initConfig.Validate(); err != nil {
			log.Error("validating configuration", "err", err)
			return
		}
		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Error("no address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address, initConfig.DNSMode)
			if err != nil {
				log.Error("config parse", "err", err)
				return
			}
		}

		cfg, err := kubevip.GenerateDaemonsetManifestFromConfig(&initConfig, image, Release.Version, inCluster, taint)
		if err != nil {
			log.Error("unable to create manifest", "err", err)
			return
		}
		fmt.Println(cfg) // output manifest to stdout
	},
}

var kubeManifestRbac = &cobra.Command{
	Use:   "rbac",
	Short: "Generate an RBAC Manifest",
	Long: `Generate the RBAC (Role-Based Access Control) manifests required for kube-vip to interact with the Kubernetes API.

kube-vip needs permissions to watch services, endpoints, configmaps, and manage leader election leases. This command outputs the minimum required ServiceAccount, Role (or ClusterRole), and the corresponding binding.

Flags:
  --role        : If true, generates a namespaced Role instead of a ClusterRole. The namespace is taken from the root --namespace flag (default: kube-system).
  --rolebinding : If true, generates a RoleBinding (if --role is also true). If --role is false, a ClusterRoleBinding is generated automatically.

The output is a multi-document YAML (separated by '---'). It is safe to apply directly:
  kube-vip manifest rbac --role --rolebinding | kubectl apply -f -

Without --role, it generates a ClusterRole and ClusterRoleBinding, which is the default behaviour and suitable for most cluster-wide deployments.`,
	Run: func(cmd *cobra.Command, _ []string) {
		initConfig.LoadBalancers = append(initConfig.LoadBalancers, initLoadBalancer)
		if err := kubevip.ParseEnvironment(&initConfig); err != nil {
			log.Error("parsing environment", "err", err)
			return
		}
		if err := initConfig.Validate(); err != nil {
			log.Error("validating configuration", "err", err)
			return
		}

		// The control plane has a requirement for a VIP being specified
		if initConfig.EnableControlPlane && (initConfig.VIP == "" && initConfig.Address == "" && !initConfig.DDNS) {
			_ = cmd.Help()
			log.Error("no address is specified for kube-vip to expose services on")
			return
		}

		// Ensure there is an address to generate the CIDR from
		if initConfig.VIPSubnet == "" && initConfig.Address != "" {
			var err error
			initConfig.VIPSubnet, err = GenerateCidrRange(initConfig.Address, initConfig.DNSMode)
			if err != nil {
				log.Error("generating VIPSubnet", "err", err)
				return
			}
		}
		saCfg := kubevip.GenerateSA(&initConfig)
		roleCfg := kubevip.GenerateRole(&initConfig, role)
		if role {
			rolebinding = true
		}
		roleBindingCfg := kubevip.GenerateRoleBinding(rolebinding, saCfg, roleCfg)

		// Output the YAML manifests to stdout
		fmt.Println("---") // Separator for YAML documents
		fmt.Println(kubevip.TransformApplyObjectToManifest(saCfg))
		fmt.Println("---") // Separator for YAML documents
		fmt.Println(kubevip.TransformApplyObjectToManifest(roleCfg))
		fmt.Println("---") // Separator for YAML documents
		fmt.Println(kubevip.TransformApplyObjectToManifest(roleBindingCfg))
	},
}
