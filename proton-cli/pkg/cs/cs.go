package cs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	slices0 "slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/constants"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/servicepackage"

	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	etcd_client "go.etcd.io/etcd/client/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	kubeletv1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/utils/clock"
	"k8s.io/utils/strings/slices"
	"sigs.k8s.io/yaml"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client"
	ecms "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/ecms/v1alpha1"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/ecms/v1alpha1/files"
	exec_v1alpha1 "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/exec/v1alpha1"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/helm3"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/node/v1alpha1"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/core/global"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/cs/addons"
	k "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/cs/kubernetes"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/cs/tiller"
)

type Cs struct {
	Logger         *logrus.Logger
	ClusterConf    *configuration.ClusterConfig
	OldClusterConf *configuration.ClusterConfig
	AllNodes       []v1alpha1.Interface
}

func (c *Cs) Apply() error {
	c.Logger.Debugf("kubernetes provisioner: %v", c.ClusterConf.Cs.Provisioner)
	switch c.ClusterConf.Cs.Provisioner {
	case configuration.KubernetesProvisionerLocal:
		c.Logger.Debug("cs setting")
		if err := c.apply(); err != nil {
			return err
		}
	case configuration.KubernetesProvisionerExternal:
		// registry, _, _ := global.ImageRepository(c.ClusterConf.Cr)
		// _, kube := client.NewK8sClient()
		// if kube == nil {
		// 	return client.ErrKubernetesClientSetNil
		// }
		// if err := tiller.Reconcile(context.Background(), kube, registry); err != nil {
		// 	return fmt.Errorf("reconcile helm tiller fail: %w", err)
		// }
		c.Logger.Debug("skip init kubernetes and tiller")
	default:
		return fmt.Errorf("invalid kubernetes provisioner: %v", c.ClusterConf.Cs.Provisioner)
	}

	// 创建必要的命名空间
	// TODO: Use const instead of magic string
	_, k := client.NewK8sClient()
	if k == nil {
		return client.ErrKubernetesClientSetNil
	}
	var resourceNamespace = configuration.GetProtonResourceNSFromFile()
	c.Logger.Printf("create resource namespace: %v", resourceNamespace)
	_, err := k.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: resourceNamespace}}, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			c.Logger.Debugf("create resource namespace %v fail: %v", resourceNamespace, err)
		} else if !apierrors.IsForbidden(err) {
			c.Logger.Debugf("create resource namespace %v fail: %v", resourceNamespace, err)
		} else {
			return fmt.Errorf("create resource namespace %v fail: %v", resourceNamespace, err)
		}
	}

	pkg := new(servicepackage.ServicePackage)
	if err := pkg.Load(global.ServicePackage); err != nil {
		c.Logger.Errorf("unable load service package: %v", err)
		return err
	}

	// 安装、更新插件
	// external 模式不需要安装
	if c.ClusterConf.Cs.Provisioner == configuration.KubernetesProvisionerLocal {
		registry, _, _ := global.ImageRepository(c.ClusterConf.Cr)
		// 插件安装在命名空间 kube-system
		helmClient, err := helm3.NewCli(metav1.NamespaceSystem, c.Logger.WithField("module", "cs"))
		if err != nil {
			c.Logger.Errorf("unable to create helm client: %v", err)
			return err
		}

		c.Logger.WithField("addons", c.ClusterConf.Cs.Addons).Info("Install CS Addons")
		for _, addon := range c.ClusterConf.Cs.Addons {
			c.Logger.WithField("name", addon).Info("Install cs addon")
			if err := addons.Reconcile(context.Background(), c.Logger, helmClient, pkg, registry, c.ClusterConf.Cs.AddonsConfig, addon); err != nil {
				c.Logger.Errorf("reconcile proton cs addon %s fail: %v", addon, err)
				return err
			}
		}
	}

	return nil
}

func (c *Cs) Reset() error {
	var provisioner configuration.KubernetesProvisioner
	// TODO: Cs 仅接受 Cs 相关配置，而非集群配置，则不需要再判断指针非空
	if c.ClusterConf != nil && c.ClusterConf.Cs != nil {
		provisioner = c.ClusterConf.Cs.Provisioner
	}
	ctx := context.Background()
	switch provisioner {
	case configuration.KubernetesProvisionerLocal:
		c.Logger.Debug("cs resetting")
		return c.resetCs()
	case configuration.KubernetesProvisionerExternal:
		c.Logger.Debug("reset external kubernetes")
		_, kube := client.NewK8sClient()
		if kube == nil {
			return client.ErrKubernetesClientSetNil
		}
		if err := tiller.RemoveTiller(ctx, kube); err != nil {
			c.Logger.Errorf("remove tiller fail: %v", err)
			return fmt.Errorf("remove tiller fail: %w", err)
		}

		var nameSpaces = []string{configuration.GetProtonResourceNSFromFile(), configuration.GetProtonCliConfigNSFromFile()}
		for _, nameSpace := range nameSpaces {
			c.Logger.Infof("remove namespace %v", nameSpace)
			if err := kube.CoreV1().Namespaces().Delete(ctx, nameSpace, metav1.DeleteOptions{}); apierrors.IsNotFound(err) {
				c.Logger.Debugf("namespaces/%v is not found", nameSpace)
			} else if err != nil {
				c.Logger.Errorf("delete namespace/%v fail: %v", nameSpace, err)
				return fmt.Errorf("delete namespace/%v fail: %w", nameSpace, err)
			}
		}

		return nil
	default:
		return fmt.Errorf("invalid kubernetes provisioner: %v", provisioner)
	}
}
func (c *Cs) apply() error {
	var cr, _, _ = global.ImageRepository(c.ClusterConf.Cr)

	chartRepo := (*k.ChartmuseumInfo)(nil)
	if c.ClusterConf.Cr.UseChartmuseum() {
		repoUrl, repoUsername, repoPassword := global.Chartmuseum(c.ClusterConf.Cr)
		chartRepo = &k.ChartmuseumInfo{
			Address:  repoUrl,
			Username: repoUsername,
			Password: repoPassword,
		}
	}

	// 更新流程
	if c.OldClusterConf != nil && len(c.OldClusterConf.Nodes) != 0 {
		// 补充容器运行时配置中缺失的字段
		if c.ClusterConf.Cs.ContainerRuntime.Containerd != nil {
			fillContainerdContainerRuntimeSource(c.ClusterConf.Cs.ContainerRuntime.Containerd, c.ClusterConf.Cr.Local)
		}

		kube, err := NewKubernetesClient()
		if err != nil {
			return fmt.Errorf("unable to create kubernetes client: %w", err)
		}

		// 获取 Kubernetes 已存在的节点
		nodeList, err := kube.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("unable to list kubernetes nodes: %w", err)
		}

		// 添加缺少的 Kubernetes 节点
		if err := c.addNodes(nodeList, chartRepo); err != nil {
			return fmt.Errorf("unable to add nodes: %w", err)
		}

		// 删除多余的 Kubernetes 节点
		if err := c.deleteNodes(nodeList); err != nil {
			return fmt.Errorf("unable to delete nodes: %w", err)
		}

		// update controller and kubelet config
		// update kubelet maxPods to 256, nodeStatusUpdateFrequency=3s
		// update kube-system kubelet-VERSION ConfigMap
		// update kube-controller-manager.yaml node-monitor-period=2s and node-monitor-grace-period=12s
		if err := c.setKubeletConfigMap(kube); err != nil {
			return fmt.Errorf("unable to update kubelet configmap: %w", err)
		}
		// update all nodes /var/lib/kubelet/config.yaml, /etc/kubermetes/manifests/kube-controller-manager.yaml and restart kubelet
		//update docker daemon.config insecure-registries when cr hosts changed
		for _, node := range c.ClusterConf.Nodes {
			sshConf := client.RemoteClientConf{
				Host:     node.IP(),
				HostName: node.Name,
			}
			if err := c.updateContainerRuntime(sshConf); err != nil {
				return fmt.Errorf("update container runtime fai: %w", err)
			}
			if err = c.setControllerManagerConfig(sshConf); err != nil {
				return fmt.Errorf("unable to update kube-controller-manager config: %w", err)
			}
			if err = c.setKubeletConfig(sshConf); err != nil {
				return fmt.Errorf("unable to update kubelet config: %w", err)
			}
		}

		return nil
	}

	kc := &k.KubernetesCluster{
		Logger:       c.Logger,
		BIP:          c.ClusterConf.Cs.Host_network.Bip,
		ETCDDataDir:  c.ClusterConf.Cs.Etcd_data_dir,
		LoadBalancer: fmt.Sprintf("proton-cs.lb.aishu.cn:%d", c.ClusterConf.Cs.Ha_port),
		ChartRepo:    chartRepo,
		// 容器运行时
		ContainerRuntime: &c.ClusterConf.Cs.ContainerRuntime,
	}
	workerWithIPs := getKubeWorkers(c.ClusterConf.Nodes, c.ClusterConf.Cs.IPFamilies)
	for _, each := range workerWithIPs {
		worker, err := k.NewNode(c.Logger, each, each, each)
		if err != nil {
			return err
		}
		kc.Workers = append(kc.Workers, *worker)
	}

	masterWithIPs := getKubeMasters(c.ClusterConf.Cs.Master, c.ClusterConf.Nodes, c.ClusterConf.Cs.IPFamilies)
	for _, each := range masterWithIPs {
		master, err := k.NewNode(c.Logger, each, each, each)
		if err != nil {
			return err
		}
		kc.Masters = append(kc.Masters, *master)
	}

	// 探查节点的容器运行时，并补充缺失的配置字段
	c.Logger.Info("detect node common container runtime")
	r, err := detectNodeCommonContainerRuntime(kc)
	if err != nil {
		return err
	}
	var containerdRoot string
	if c.ClusterConf.Cs.ContainerRuntime.Containerd != nil {
		containerdRoot = c.ClusterConf.Cs.ContainerRuntime.Containerd.Root
	}
	// generateContainerRuntimeSourceInto 会补充缺失的字段（如 sandbox_image 和 registries）
	generateContainerRuntimeSourceInto(r, &c.ClusterConf.Cs.ContainerRuntime, c.ClusterConf.Cr.Local, containerdRoot)
	c.Logger.WithField("container-runtime", c.ClusterConf.Cs.ContainerRuntime)

	for cidr := range strings.SplitSeq(c.ClusterConf.Cs.Host_network.Pod_network_cidr, ",") {
		if strings.Contains(cidr, ":") {
			kc.IPv6PodCIDR = cidr
		} else {
			kc.IPv4PodCIDR = cidr
		}
	}

	for cidr := range strings.SplitSeq(c.ClusterConf.Cs.Host_network.Service_cidr, ",") {
		if strings.Contains(cidr, ":") {
			kc.IPv6ServiceCIDR = cidr
		} else {
			kc.IPv4ServiceCIDR = cidr
		}
	}
	crHost := strings.Split(cr, "/")[0]
	kc.InsecureRegistries = append(kc.InsecureRegistries, crHost)
	if l := c.ClusterConf.Cr.Local; l != nil {
		for _, n := range c.ClusterConf.Nodes {
			kc.InsecureRegistries = append(kc.InsecureRegistries, net.JoinHostPort(n.Name, strconv.Itoa(l.Ports.Registry)))
		}
	}
	sort.Strings(kc.InsecureRegistries)
	kc.ImageRepository = cr
	if c.ClusterConf.Cs.Host_network.Ipv4_interface != "" {
		kc.IPv4Interface = c.ClusterConf.Cs.Host_network.Ipv4_interface
	}
	if c.ClusterConf.Cs.Host_network.Ipv6_interface != "" {
		kc.IPv6Interface = c.ClusterConf.Cs.Host_network.Ipv6_interface
	}

	if err := kc.InitCluster(); err != nil {
		kc.Logger.Errorf("init kubernetes cluster fail: %v", err)
		return err
	}

	err = c.initCs()

	c.Logger.Info("init proton cs end")

	return err
}

func (c *Cs) initCs() error {
	var err error
	_, clientSet := client.NewK8sClient()
	if clientSet == nil {
		return client.ErrKubernetesClientSetNil
	}
	if !IsKubernetesAPIReady(clientSet, clock.RealClock{}) {
		return fmt.Errorf("kubernetes api is not ready")
	}
	ctx := context.Background()

	// proton 组件namespace
	namespaceClient := clientSet.CoreV1().Namespaces()

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: configuration.GetProtonCliConfigNSFromFile(),
		},
	}
	// 创建 namespace
	_, err = namespaceClient.Create(ctx, namespace, metav1.CreateOptions{})
	if err != nil {
		return err
	} else {
		c.Logger.Debug(fmt.Sprintf("create namespace %s success", namespace.Name))
	}

	// get admin.conf from control plane nodes to run kubectl and proton-cli on worker nodes
	masterAdminConf, err := c.getAdminConfFromMasterNode()
	if err != nil {
		return fmt.Errorf("unable to get kubernetes admin.conf from master nodes: %w", err)
	}

	// update controller and kubelet config
	// update kubelet maxPods to 256, nodeStatusUpdateFrequency=3s
	// update kube-system kubelet-VERSION ConfigMap
	// update kube-controller-manager.yaml node-monitor-period=2s and node-monitor-grace-period=12s
	// copy control plane nodes' /etc/kubernetes/admin.conf to all worker nodes' /root/.kube/config (overwrite)
	if err := c.setKubeletConfigMap(clientSet); err != nil {
		return fmt.Errorf("unable to update kubelet configmap: %w", err)
	}
	// update all nodes /var/lib/kubelet/config.yaml, /etc/kubermetes/manifests/kube-controller-manager.yaml and restart kubelet
	for _, node := range c.ClusterConf.Nodes {
		sshConf := client.RemoteClientConf{
			Host:     node.IP(),
			HostName: node.Name,
		}
		if err = c.setControllerManagerConfig(sshConf); err != nil {
			return fmt.Errorf("unable to update kube-controller-manager config: %w", err)
		}
		if err = c.setKubeletConfig(sshConf); err != nil {
			return fmt.Errorf("unable to update kubelet config: %w", err)
		}
	}
	if err = c.copyAdminConfToWorkerNode(masterAdminConf); err != nil {
		return fmt.Errorf("unable to copy admin.conf to worker nodes: %w", err)
	}

	return nil
}

func (c *Cs) getAdminConfFromMasterNode() (string, error) {
	var ctx = context.TODO()
	masterAdminConf := ""
	for _, nodeInterface := range c.AllNodes {
		for _, masterName := range c.ClusterConf.Cs.Master {
			if nodeInterface.Name() == masterName {
				adminConfBytes, err := nodeInterface.ECMS().Files().ReadFile(ctx, global.K8SAdminConfPath)
				if err != nil {
					return "", fmt.Errorf("unable to read admin.conf from control plane node: %w", err)
				}
				adminConf := string(adminConfBytes)
				if masterAdminConf == "" {
					masterAdminConf = adminConf
				} else if adminConf != masterAdminConf {
					c.Logger.Warningln("/etc/kubernetes/admin.conf differs among control plane nodes.")
				}
			}
		}
	}
	if masterAdminConf == "" {
		return "", fmt.Errorf("acquired kubernetes admin.conf was empty on all master nodes")
	}
	return masterAdminConf, nil
}

func (c *Cs) updateContainerRuntime(conf client.RemoteClientConf) error {
	switch {
	case c.ClusterConf.Cs.ContainerRuntime.Containerd != nil:
		n, err := k.NewNode(c.Logger, conf.HostName, conf.Host, conf.Host)
		if err != nil {
			return err
		}
		return n.InitContainerd(c.ClusterConf.Cs.ContainerRuntime.Containerd)
	default:
		return nil
	}
}

func (c *Cs) setKubeletConfigMap(kube kubernetes.Interface) error {
	const kubeletCMName = "kubelet-config"

	kubeCm, err := kube.CoreV1().ConfigMaps("kube-system").Get(context.Background(), kubeletCMName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	kubeletCMConf := kubeCm.Data["kubelet"]
	kubeletConfig := new(kubeletv1.KubeletConfiguration)
	if err := yaml.Unmarshal([]byte(kubeletCMConf), kubeletConfig); err != nil {
		return err
	}

	if kubeletConfig.MaxPods < global.KubeletMaxPods || kubeletConfig.EvictionHard["nodefs.available"] != global.KubeletEvictNodefsAva || kubeletConfig.NodeStatusUpdateFrequency.Duration != global.NodeStatusUpdateFrequency*time.Second {
		kubeletConfig.NodeStatusUpdateFrequency.Duration = global.NodeStatusUpdateFrequency * time.Second
		kubeletConfig.MaxPods = global.KubeletMaxPods
		if kubeletConfig.EvictionHard == nil {
			kubeletConfig.EvictionHard = map[string]string{}
		}
		kubeletConfig.EvictionHard["nodefs.available"] = global.KubeletEvictNodefsAva
		kubeletConfigByte, err := yaml.Marshal(kubeletConfig)
		if err != nil {
			return fmt.Errorf("unable to encode kubelet config: %w", err)
		}
		kubeCm.Data["kubelet"] = string(kubeletConfigByte)
		_, err = kube.CoreV1().ConfigMaps("kube-system").Update(context.Background(), kubeCm, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("unable to update kubelet config configmap: %w", err)
		}
	}
	return nil
}

func (c *Cs) setKubeletConfig(conf client.RemoteClientConf) error {
	var ctx = context.TODO()
	var e = exec_v1alpha1.NewECMSExecutorForHost(ecms.NewForHost(conf.Host).Exec())
	var f = ecms.NewForHost(conf.Host).Files()
	cfgContent, err := f.ReadFile(ctx, "/var/lib/kubelet/config.yaml")
	if err != nil {
		return err
	}
	var kubeletYaml *kubeletv1.KubeletConfiguration
	if err := yaml.Unmarshal(cfgContent, &kubeletYaml); err != nil {
		return err
	}
	var needRestartKubelet bool
	if kubeletYaml.NodeStatusUpdateFrequency.Duration != global.NodeStatusUpdateFrequency*time.Second {
		kubeletYaml.NodeStatusUpdateFrequency.Duration = global.NodeStatusUpdateFrequency * time.Second

		needRestartKubelet = true
	}

	if kubeletYaml.MaxPods < global.KubeletMaxPods || kubeletYaml.EvictionHard["nodefs.available"] != global.KubeletEvictNodefsAva {
		kubeletYaml.MaxPods = global.KubeletMaxPods
		if kubeletYaml.EvictionHard == nil {
			kubeletYaml.EvictionHard = map[string]string{}
		}
		kubeletYaml.EvictionHard["nodefs.available"] = global.KubeletEvictNodefsAva

		needRestartKubelet = true
	}

	if needRestartKubelet {
		kubeletYamlByte, err := yaml.Marshal(kubeletYaml)
		if err != nil {
			return err
		}
		if err := f.Create(ctx, "/var/lib/kubelet/config.yaml", false, kubeletYamlByte); err != nil {
			return err
		}

		cmd := "systemctl restart kubelet"
		c.Logger.Info("run cmd ", cmd)
		if err := e.Command("systemctl", "restart", "kubelet").Run(); err != nil {
			return err
		}
	}
	return nil
}

// set node-monitor-period and node-monitor-grace-period
func (c *Cs) setControllerManagerConfig(conf client.RemoteClientConf) error {
	var ctx = context.TODO()
	var f = ecms.NewForHost(conf.Host).Files()

	controllerCfgBytes, err := f.ReadFile(ctx, "/etc/kubernetes/manifests/kube-controller-manager.yaml")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	controller := &corev1.Pod{}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(controllerCfgBytes), 100).Decode(controller); err != nil {
		return err
	}

	if !slices.Contains(controller.Spec.Containers[0].Command, global.NodeMonitorPeriod) {
		controller.Spec.Containers[0].Command = append(controller.Spec.Containers[0].Command, global.NodeMonitorPeriod)
	}
	if !slices.Contains(controller.Spec.Containers[0].Command, global.NodeMonitorGracePeriod) {
		controller.Spec.Containers[0].Command = append(controller.Spec.Containers[0].Command, global.NodeMonitorGracePeriod)
	}

	controllerCfgByte, err := yaml.Marshal(controller)
	if err != nil {
		return err
	}
	if err := f.Create(ctx, "/etc/kubernetes/manifests/kube-controller-manager.yaml", false, controllerCfgByte); err != nil {
		return err
	}

	return nil
}

func (c *Cs) copyAdminConfToWorkerNode(data string) error {
	var ctx = context.TODO()
	K8SAdminConfDestDir := filepath.Join(global.RootHomeDir, clientcmd.RecommendedHomeDir)
	K8SAdminConfDestination := filepath.Join(K8SAdminConfDestDir, clientcmd.RecommendedFileName)
	for _, nodeInterface := range c.AllNodes {
		if slices.Contains(c.ClusterConf.Cs.Master, nodeInterface.Name()) {
			continue
		}
		_, err := nodeInterface.ECMS().Files().Stat(ctx, K8SAdminConfDestDir)
		if errors.Is(err, fs.ErrNotExist) {
			err1 := nodeInterface.ECMS().Files().Create(ctx, K8SAdminConfDestDir, true, nil)
			if err1 != nil {
				return fmt.Errorf("unable to create %s folder at node %s", K8SAdminConfDestDir, nodeInterface.Name())
			}
		} else if err != nil {
			return fmt.Errorf("unable to stat the kubectl config file on node %s: %w", nodeInterface.Name(), err)
		}
		_, err = nodeInterface.ECMS().Files().Stat(ctx, K8SAdminConfDestination)
		if err == nil {
			c.Logger.Warningf("A K8S Kubectl access config file at %s already existed on node %s", K8SAdminConfDestination, nodeInterface.Name())
			err1 := nodeInterface.ECMS().Files().Delete(ctx, K8SAdminConfDestination+".bak")
			if err1 != nil && (!errors.Is(err1, fs.ErrNotExist)) {
				return fmt.Errorf("unable to remove old kube access config backup: %w", err1)
			}
			err1 = nodeInterface.ECMS().Files().Rename(ctx, K8SAdminConfDestination, K8SAdminConfDestination+".bak")
			if err1 != nil {
				return fmt.Errorf("unable to rename old kube access config to add backup suffix: %w", err1)
			}
		}
		if err := nodeInterface.ECMS().Files().Create(ctx, K8SAdminConfDestination, false, []byte(data)); err != nil {
			return fmt.Errorf("unable to write to node %s: %w", nodeInterface.Name(), err)
		}
		// TODO: chmod 0600 $HOME/.kube/config
	}
	return nil
}

func (c *Cs) resetCs() error {
	var ctx = context.TODO()

	// parallel for nodes
	g, ctx := errgroup.WithContext(ctx)
	for _, n := range c.AllNodes {
		g.Go(func() error {
			e := exec_v1alpha1.NewECMSExecutorForHost(n.ECMS().Exec())
			// disable and stop systemd unit kubelet.service
			c.Logger.WithField("node", n.IP()).Info("disable and stop systemd unit kubelet.service")
			if err := e.Command("systemctl", "disable", "--now", "kubelet.service").Run(); err != nil {
				return err
			}
			// umount directories under /var/lib/kubelet
			c.Logger.WithField("node", n.IP()).Info("umount directories under /var/lib/kubelet")
			out, err := n.ECMS().Files().ReadFile(ctx, "/proc/mounts")
			if err != nil {
				return err
			}
			for s := bufio.NewScanner(bytes.NewReader(out)); s.Scan(); {
				fields := strings.Fields(s.Text())
				if len(fields) < 2 {
					c.Logger.WithField("node", n.IP()).WithField("line", s.Text()).Warning("ignore unsupported line of /proc/mounts")
					continue
				}
				// mount point
				var p = fields[1]
				if !strings.HasPrefix(p, "/var/lib/kubelet/") {
					continue
				}
				if err := e.Command("umount", p).Run(); err != nil {
					return err
				}
			}
			// kubeadm reset
			c.Logger.WithField("node", n.IP()).Info("reset kubernetes via kubeadm")
			if err := e.Command("kubeadm", "reset", "--force").Run(); err != nil {
				return err
			}
			// remove /root/.proton-cli.yaml
			c.Logger.WithField("node", n.IP()).Info("remove /root/.proton-cli.yaml")
			if err := n.ECMS().Files().Delete(ctx, "/root/.proton-cli.yaml"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			// 清理 Kubernetes 相关的 IPTables 规则
			c.Logger.WithField("node", n.Name()).Info("clean iptables")
			if err := cleanNodeIPtables(exec_v1alpha1.NewECMSExecutorForHost(n.ECMS().Exec())); err != nil {
				return err
			}

			return nil
		})
	}

	return g.Wait()
}

// SetControlPlane 修改 Kubernetes 的 Control Plane
func (c *Cs) SetControlPlane() error {
	ctx := context.TODO()
	// nodes 是节点名称与 IP 地址的映射，一个节点的多个 IP 地址以 `,` 分隔
	var nodes = make(map[string]string)
	for _, n := range c.ClusterConf.Nodes {
		var ips []string
		if n.IP4 != "" {
			ips = append(ips, n.IP4)
		}
		if n.IP6 != "" {
			ips = append(ips, n.IP6)
		}
		nodes[n.Name] = strings.Join(ips, ",")
	}
	// create kubernetes client
	kube, err := NewKubernetesClient()
	if err != nil {
		return fmt.Errorf("unable to create kubernetes client: %w", err)
	}
	// get a control plane (master) node to execute kubectl
	masters := getKubeMasters(lo.Intersect(c.ClusterConf.Cs.Master, c.OldClusterConf.Cs.Master), c.ClusterConf.Nodes, c.ClusterConf.Cs.IPFamilies)
	if len(masters) == 0 {
		return errors.New("all master nodes will be removed")
	}
	var masterNodeName string
	for _, n := range c.ClusterConf.Nodes {
		if n.IP4 == masters[0] {
			masterNodeName = n.Name
			break
		}
		if n.IP6 == masters[0] {
			masterNodeName = n.Name
			break
		}
	}
	if masterNodeName == "" {
		return fmt.Errorf("node name of ip %q is not found", masters[0])
	}

	master, err := k.NewNode(c.Logger, masterNodeName, masters[0], masters[0])
	if err != nil {
		return err
	}
	executor := exec_v1alpha1.NewECMSExecutorForHost(master.ECMS.Exec())

	// check all nodes are ready
	nodeList, err := kube.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("unable to list kubernetes nodes: %w", err)
	}
	for _, n := range nodeList.Items {
		for _, c := range n.Status.Conditions {
			if c.Type != "Ready" {
				continue
			}
			if c.Status == corev1.ConditionTrue {
				continue
			}
			return fmt.Errorf("node %q is not ready: %s", n.Name, c.Message)
		}
	}

	// del control plane nodes
	{
		// the nodes will be change control plane -> worker
		var nodes = lo.Filter(c.AllNodes, func(n v1alpha1.Interface, _ int) bool {
			return lo.Contains(c.OldClusterConf.Cs.Master, n.Name()) && !lo.Contains(c.ClusterConf.Cs.Master, n.Name())
		})
		// get etcd pod to remove etcd members
		fieldSelector := fields.SelectorFromSet(fields.Set{"spec.nodeName": master.HostName})
		labelSelector := labels.SelectorFromSet(labels.Set{"tier": "control-plane", "component": "etcd"})
		pl, err := kube.CoreV1().Pods(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{LabelSelector: labelSelector.String(), FieldSelector: fieldSelector.String()})
		if err != nil {
			return err
		}
		if len(pl.Items) == 0 {
			c.Logger.WithField("field-selector", fieldSelector).WithField("label-selector", labelSelector).Error("unable to get etcd pod to remove etcd members")
			return errors.New("unable to get etcd pod to remove etcd members")
		}
		pod := &pl.Items[0]

		// get current etcd members
		out, err := executor.Command("kubectl", "exec", pod.Name, "--namespace", pod.Namespace, "--", "etcdctl", "--cacert=/etc/kubernetes/pki/etcd/ca.crt", "--cert=/etc/kubernetes/pki/etcd/peer.crt", "--key=/etc/kubernetes/pki/etcd/peer.key", "--write-out=json", "member", "list").Output()
		if err != nil {
			return err
		}
		var resp etcd_client.MemberListResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			return err
		}

		// remove etcd members
		var members []string
		for _, m := range resp.Members {
			if lo.Contains(c.ClusterConf.Cs.Master, m.Name) {
				continue
			}
			c.Logger.WithField("id", m.ID).WithField("name", m.Name).Info("remove etcd members")
			var args = []string{
				"exec",
				pod.Name,
				"--namespace",
				pod.Namespace,
				"--",
				"etcdctl",
				"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
				"--cert=/etc/kubernetes/pki/etcd/peer.crt",
				"--key=/etc/kubernetes/pki/etcd/peer.key",
				"member",
				"remove",
				m.Name,
			}
			args = append(args, members...)
			if err := executor.Command("kubectl", args...).Run(); err != nil {
				return err
			}
		}

		// remove static pods
		for _, n := range nodes {
			for _, p := range []string{
				"/etc/kubernetes/manifests/etcd.yaml",
				"/etc/kubernetes/manifests/kube-apiserver.yaml",
				"/etc/kubernetes/manifests/kube-controller-manager.yaml",
				"/etc/kubernetes/manifests/kube-scheduler.yaml",
			} {
				if err := n.ECMS().Files().Delete(ctx, p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}

		// remove node labels
		for _, n := range nodeList.Items {
			if lo.Contains(c.ClusterConf.Cs.Master, n.Name) {
				continue
			}
			// TODO: reference constant string instead of magic string
			delete(n.Labels, "node-role.kubernetes.io/control-plane")
			delete(n.Labels, "node-role.kubernetes.io/master")
			c.Logger.WithField("node", n.Name).Info("remove kubernetes control plane (master) labels from nodes that are no logger control plane (master)")
			if _, err := kube.CoreV1().Nodes().Update(ctx, &n, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
	}

	// add control plane nodes
	{
		// the nodes will be change worker -> control plane
		var nodes = lo.Filter(c.AllNodes, func(n v1alpha1.Interface, _ int) bool {
			return !lo.Contains(c.OldClusterConf.Cs.Master, n.Name()) && lo.Contains(c.ClusterConf.Cs.Master, n.Name())
		})

		// generate token and certificate for kubeadm join
		token, certHash, certKey, err := master.GetJoinKeys(true)
		if err != nil {
			return fmt.Errorf("get kubeadm join cluster master role command failed: %v", err)
		}

		for _, n := range nodes {
			// executor
			var executor = exec_v1alpha1.NewECMSExecutorForHost(n.ECMS().Exec())

			// kubeadm join phase control-plane-prepare all
			{
				var args = []string{
					"join",
					"phase",
					"control-plane-prepare",
					"all",
					net.JoinHostPort(global.ProtonCsDomain, strconv.Itoa(c.ClusterConf.Cs.Ha_port)),
					fmt.Sprintf("--apiserver-advertise-address=%s", n.IP()),
					fmt.Sprintf("--apiserver-bind-port=%d", global.KubernetesAPIServerBindPort),
					fmt.Sprintf("--certificate-key=%s", certKey),
					"--control-plane",
					fmt.Sprintf("--discovery-token-ca-cert-hash=%s", certHash),
					fmt.Sprintf("--node-name=%s", n.Name()),
					fmt.Sprintf("--token=%s", token),
				}
				c.Logger.WithField("node", n.Name()).Info("execute kubeadm join phase control-plane-prepare all")
				if err := executor.Command("kubeadm", args...).Run(); err != nil {
					return err
				}
			}

			// kubeadm join phase control-plane-join
			{
				var args = []string{
					"join",
					"phase",
					"control-plane-join",
					"all",
					"--control-plane",
					fmt.Sprintf("--apiserver-advertise-address=%s", n.IP()),
					fmt.Sprintf("--node-name=%s", n.Name()),
				}
				c.Logger.WithField("node", n.Name()).Info("execute kubeadm join phase control-plane-join all")
				if err := executor.Command("kubeadm", args...).Run(); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// hostFromNode 在 []configuration.Node 中查找指定节点对应的 IP 地址，多个 IP 地址以 `,` 分隔
func hostFromNodes(name string, nodes []configuration.Node) (string, bool) {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		var addr []string
		if n.IP4 != "" {
			addr = append(addr, n.IP4)
		}
		if n.IP6 != "" {
			addr = append(addr, n.IP6)
		}
		return strings.Join(addr, ","), true
	}
	return "", false
}

// addNodes 添加 ClusterConf 中已配置，但 Kubernetes 不存在的节点
func (c *Cs) addNodes(list *corev1.NodeList, chartRepo *k.ChartmuseumInfo) error {
	// 待添加的 master
	var joinMasters []k.Node
	for _, m := range c.ClusterConf.Cs.Master {
		var existed bool
		for _, n := range list.Items {
			// 跳过非 control plane/master 节点
			_, label := n.Labels[LabelNodeRoleControlPlane]
			_, labelOld := n.Labels[LabelNodeRoleOldControlPlane]
			if !label && !labelOld {
				continue
			}

			if n.Name == m {
				existed = true
				break
			}
		}
		if existed {
			continue
		}

		var n configuration.Node
		for _, nn := range c.ClusterConf.Nodes {
			if nn.Name == m {
				n = nn
				break
			}
		}

		kn, err := k.NewNode(c.Logger, n.Name, n.IP(), n.IP())
		if err != nil {
			return err
		}
		joinMasters = append(joinMasters, *kn)
	}
	// 待添加的 worker
	var joinWorkers []k.Node
	for _, w := range c.ClusterConf.Nodes {
		var existed bool
		for _, n := range list.Items {
			if n.Name == w.Name {
				existed = true
				break
			}
		}
		if existed {
			continue
		}

		// 跳过 master 节点
		if slices.Contains(c.ClusterConf.Cs.Master, w.Name) {
			continue
		}

		var n configuration.Node
		for _, nn := range c.ClusterConf.Nodes {
			if nn.Name == w.Name {
				n = nn
				break
			}
		}

		kn, err := k.NewNode(c.Logger, n.Name, n.IP(), n.IP())
		if err != nil {
			return err
		}
		joinWorkers = append(joinWorkers, *kn)
	}

	if len(joinMasters)+len(joinWorkers) == 0 {
		c.Logger.Info("no need to add kubernetes node")
		return nil
	}

	// 所有待添加节点初始化操作系统配置
	for _, n := range append(joinMasters, joinWorkers...) {
		c.Logger.WithField("name", n.HostName).Info("Initial OS")
		if err := n.InitOS(); err != nil {
			return fmt.Errorf("initial %s node envirionments failed: %v", n.Ipaddress, err)
		}
		if err := n.InitialNodeInfo(); err != nil {
			return fmt.Errorf("initial %s node envirionments failed: %v", n.Ipaddress, err)
		}
		if err := n.InitialContainerRuntime(&c.ClusterConf.Cs.ContainerRuntime); err != nil {
			return fmt.Errorf("initial %s node container runtime failed: %v", n.Ipaddress, err)
		}
	}

	// get first master node to generate token and certificate for kubeadm join
	var master *k.Node
	{
		m := getKubeMasters(c.ClusterConf.Cs.Master, c.ClusterConf.Nodes, c.ClusterConf.Cs.IPFamilies)[0]
		n, err := k.NewNode(c.Logger, m, m, m)
		if err != nil {
			return err
		}
		master = n
	}

	// 逐个添加 control-plane/master
	{
		// generate token and certificate for kubeadm join on control-plane/master node
		token, certHash, certKey, err := master.GetJoinKeys(true)
		if err != nil {
			return fmt.Errorf("unable to token and certificate: %v", err)
		}
		for _, n := range joinMasters {
			c.Logger.WithField("name", n.HostName).Info("Add kubernetes control-plane/master node")
			joinMasterConfig, err := k.GenerateKubeadmJoinYAML(
				&c.ClusterConf.Cs.ContainerRuntime,
				net.JoinHostPort(LocalKubernetesControlPlaneEndpointHost, strconv.Itoa(c.ClusterConf.Cs.Ha_port)),
				token,
				certHash,
				certKey,
				n.Ipaddress,
			)
			if err != nil {
				return fmt.Errorf("get kubeadm join cluster master role command failed: %v", err)
			}
			if err := n.RewriteKubeletConfig(); err != nil {
				return fmt.Errorf("%s: rewrite kubelet config failed: %v", n.HostName, err)
			}
			if err := n.JoinKubernetesWithYaml(joinMasterConfig); err != nil {
				return fmt.Errorf("%s: join to cluster master role failed: %v", n.HostName, err)
			}
			if err := n.EnableKubeletService(); err != nil {
				return fmt.Errorf("%s: enable kubelet service failed: %v", n.HostName, err)
			}
		}
		// 移除新增 control plane 节点的 taint
		master.RemoveTaint(joinMasters)
	}

	// 添加 worker
	{
		// generate token and certificate for kubeadm join on worker node
		token, certHash, certKey, err := master.GetJoinKeys(false)
		if err != nil {
			return fmt.Errorf("unable to token and certificate: %v", err)
		}
		for _, n := range joinWorkers {
			c.Logger.WithField("name", n.HostName).Info("Add kubernetes worker node")
			joinWorkerConfig, err := k.GenerateKubeadmJoinYAML(
				&c.ClusterConf.Cs.ContainerRuntime,
				net.JoinHostPort(LocalKubernetesControlPlaneEndpointHost, strconv.Itoa(c.ClusterConf.Cs.Ha_port)),
				token,
				certHash,
				certKey,
				n.Ipaddress,
			)
			if err != nil {
				return fmt.Errorf("%s: get join kubernetes kubeadm config: %w", n.Ipaddress, err)
			}
			if err := n.RewriteKubeletConfig(); err != nil {
				return fmt.Errorf("%s: rewrite kubelet config: %w", n.Ipaddress, err)
			}
			if err := n.JoinKubernetesWithYaml(joinWorkerConfig); err != nil {
				return fmt.Errorf("%s:join kubernetes cluster : %w", n.Ipaddress, err)
			}
			if err := n.EnableKubeletService(); err != nil {
				return fmt.Errorf("%s: enable kubelet service: %w", n.Ipaddress, err)
			}
		}
	}

	return nil
}

// deleteNodes 删除 ClusterConf 中未配置，但 Kubernetes 存在的节点
func (c *Cs) deleteNodes(list *corev1.NodeList) error {
	// 已有节点的名称列表
	var actualNames = sets.New[string]()
	for _, n := range list.Items {
		actualNames.Insert(n.Name)
	}
	// 期望的节点名称列表
	var expectNames = sets.New[string]()
	for _, n := range c.ClusterConf.Nodes {
		expectNames.Insert(n.Name)
	}
	// 待删除节点的名称列表
	var names = sets.List(actualNames.Difference(expectNames))
	if len(names) == 0 {
		c.Logger.Debug("no need to delete node")
		return nil
	}

	master := filterControlPlaneNode(list)
	if master == nil {
		return errors.New("control plane node not found")
	}

	// master 节点的 IP 地址。双栈网络，优先使用 IPv4 地址。
	var host string
	for _, n := range c.ClusterConf.Nodes {
		if n.Name != master.Name {
			continue
		}
		if n.IP4 != "" {
			host = n.IP4
		} else if n.IP6 != "" {
			host = n.IP6
		}
		break
	}
	if host == "" {
		return fmt.Errorf("ip of master node %q not found", master.Name)
	}

	executor := exec_v1alpha1.NewECMSExecutorForHost(ecms.NewForHost(host).Exec())

	for _, name := range names {
		if err := executor.Command("kubectl", "cordon", name).Run(); err != nil {
			return fmt.Errorf("unable to cordon kubernetes node %s: %w", name, err)
		}
	}

	for _, name := range names {
		if err := executor.Command(
			"kubectl", "drain", name,
			"--ignore-daemonsets=true",
			"--skip-wait-for-delete-timeout=30",
			"--delete-emptydir-data=true",
			"--timeout=300s",
		).Run(); err != nil {
			return fmt.Errorf("unable to drain kubernetes node %s: %w", name, err)
		}
	}

	var args []string
	args = append(args, "delete", "node")
	args = append(args, names...)
	if err := executor.Command("kubectl", args...).Run(); err != nil {
		return fmt.Errorf("unable to delete kubernetes nodes %s: %w", strings.Join(names, ", "), err)
	}

	{
		// 缩容节点后延迟等待一段时间
		delayTime, err := strconv.Atoi(os.Getenv("PROTON_CLI_NODE_REMOVE_DELAY"))
		if err != nil {
			delayTime = 60
		}
		time.Sleep(time.Duration(delayTime) * time.Second)
	}

	return nil
}

// IsControlPlaneChanged 检查 Control Plane 是否添加或删除节点，顺序不敏感
func IsControlPlaneChanged(new, old []string) bool {
	if len(new) != len(old) {
		return true
	}

	var nodeExists = func(node string, nodes []string) bool {
		return slices0.Contains(nodes, node)
	}

	for _, n := range new {
		if !nodeExists(n, old) {
			return true
		}
	}
	return false
}

// DetectNodeCommonCRISocket 探查所有节点都有的容器运行时
func DetectNodeCommonCRISocket(nodes []k.Node) (string, error) {
	var found = sets.New[string]()

	for _, n := range nodes {
		sockets, err := detectNodeCRISockets(&n)
		if err != nil {
			return "", err
		}
		found.Insert(sockets...)
	}

	if found.Has(constants.CRISocketContainerd) {
		return constants.CRISocketContainerd, nil
	}

	return "", fmt.Errorf("CRISocket not found")
}

// detectNodeCRISockets 探查节点的容器运行时，返回已知的容器运行时列表。
func detectNodeCRISockets(n *k.Node) ([]string, error) {
	var found []string
	ok, err := isNodeExistingSocket(n.ECMS.Files(), constants.CRISocketContainerd)
	if err != nil {
		return nil, err
	}
	if ok {
		found = append(found, constants.CRISocketContainerd)
	}
	return found, nil
}

// isNodeExistingSocket 判断节点的指定路径是否为 socket 文件
func isNodeExistingSocket(c files.Interface, path string) (bool, error) {
	var ctx = context.TODO()
	u, err := url.Parse(path)
	if err != nil {
		return false, err
	}

	info, err := c.Stat(ctx, u.Path)
	log.Printf("DEBUG, %s, %v, %v", path, err, info)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode()&fs.ModeSocket != 0, nil
}

func getKubeMasters(masters []string, nodes []configuration.Node, ipFamilies []corev1.IPFamily) []string {
	var kubeMasters []string
	for _, name := range masters {
		for _, node := range nodes {
			if name != node.Name {
				continue
			}
			// 优先使用 IPv4 地址
			if lo.Contains(ipFamilies, corev1.IPv4Protocol) && node.IP4 != "" {
				kubeMasters = append(kubeMasters, node.IP4)
			} else if lo.Contains(ipFamilies, corev1.IPv6Protocol) && node.IP6 != "" {
				// 只有在没有 IPv4 地址或不支持 IPv4 时才使用 IPv6
				ip := net.ParseIP(node.IP6).To16()
				if ip != nil {
					kubeMasters = append(kubeMasters, ip.String())
				}
			}
		}
	}
	return kubeMasters
}

func getKubeWorkers(nodes []configuration.Node, ipFamilies []corev1.IPFamily) []string {
	var kubeWorkers []string
	for _, node := range nodes {
		// 优先使用 IPv4 地址
		if lo.Contains(ipFamilies, corev1.IPv4Protocol) && node.IP4 != "" {
			kubeWorkers = append(kubeWorkers, node.IP4)
		} else if lo.Contains(ipFamilies, corev1.IPv6Protocol) && node.IP6 != "" {
			// 只有在没有 IPv4 地址或不支持 IPv4 时才使用 IPv6
			ip := net.ParseIP(node.IP6).To16()
			if ip != nil {
				kubeWorkers = append(kubeWorkers, ip.String())
			}
		}
	}
	return kubeWorkers
}
