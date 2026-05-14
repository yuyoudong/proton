package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/yaml"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client"
	ecms "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/ecms/v1alpha1"
	exec "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/exec/v1alpha1"
	firewalld "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/firewalld/v1alpha1"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/core/global"
	slbops "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/slb"
)

type Node struct {
	Logger         logrus.FieldLogger
	ClusterConf    *configuration.ClusterConfig
	OldClusterConf *configuration.ClusterConfig
	HttpClient     *client.HttpClient
}

type (
	I   = []any
	MSI = map[string]any
	MII = map[any]any
)

type authStruct struct {
	sync.Mutex
	publicKeys []byte
	number     int

	// 已经完成设置的节点数，包括失败的节点。用于判断是否因该继续等待其他
	// goroutine 写入 publicKeys
	numberCompleted int
}

var auth authStruct

// node (hostname、auth、internalDomainName、slb、chrony)
func (n *Node) Apply() error {
	n.Logger.Debug("node setting")
	return n.setNodes()
}

// node (hostname、auth、internalDomainName、slb、chrony)
func (n *Node) Reset() error {
	n.Logger.Debug("node resetting")
	n.resetNodes()
	return nil
}

func (n *Node) resetNodes() {
	hosts := n.ClusterConf.Nodes

	var wg sync.WaitGroup
	for i := range hosts {
		var host string
		if hosts[i].IP4 != "" {
			host = hosts[i].IP4
		} else {
			host = hosts[i].IP6
		}
		sshConf := client.RemoteClientConf{
			Host:     host,
			HostName: hosts[i].Name,
		}
		wg.Add(1)
		go n.resetNode(&wg, sshConf)
	}

	wg.Wait()
}

func (n *Node) resetNode(wg *sync.WaitGroup, conf client.RemoteClientConf) {
	defer wg.Done()
	// executor
	e := exec.NewECMSExecutorForHost(ecms.NewForHost(conf.Host).Exec())

	n.Logger.Info(fmt.Sprintf("reset node on node:%s", conf.Host))
	// firewall
	if err := n.resetFirewall(&n.ClusterConf.Firewall, firewalld.New(e)); err != nil {
		n.Logger.WithFields(map[string]any{"err": err, "host": conf.Host}).Warn("reset firewall failed")
	}
	n.Logger.Infof("remove old proton cli dir of %v", conf.HostName)
	if err := configuration.RemoveOldProtonCLIDirIfExist(ecms.NewForHost(conf.Host).Files()); err != nil {
		n.Logger.Warningf("remove old proton cli dir of %v fail: %v", conf.HostName, err)
	}
	// 重置节点时移除内核模块持久化配置文件
	n.Logger.Infof("removing kernel modules config of %v", conf.HostName)
	if err := ecms.NewForHost(conf.Host).Files().Delete(context.TODO(), protonModulesLoadPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			n.Logger.Warningf("removing kernel modules config of %v fail: %v", conf.HostName, err)
		}
	}
}

func (n *Node) setNodes() error {
	hosts := n.ClusterConf.Nodes
	var wg sync.WaitGroup
	auth = authStruct{
		publicKeys: []byte{},
		number:     0,
	}
	var errList []error
	for i := range hosts {
		var host string
		if hosts[i].IP4 != "" {
			host = hosts[i].IP4
		} else {
			host = hosts[i].IP6
		}
		sshConf := client.RemoteClientConf{
			Host:     host,
			HostName: hosts[i].Name,
		}
		wg.Add(1)
		go func(sshConf client.RemoteClientConf) {
			defer wg.Done()
			if err := n.setNode(sshConf); err != nil {
				errList = append(errList, fmt.Errorf("set node %s fail: %w", sshConf.HostName, err))
			}
		}(sshConf)
	}
	wg.Wait()

	return utilerrors.NewAggregate(errList)
}

func (n *Node) setNode(conf client.RemoteClientConf) error {
	var ctx = context.TODO()
	defer func() {
		auth.Lock()
		defer auth.Unlock()
		auth.numberCompleted++
	}()

	ecmsV1Alpha1 := ecms.NewForHost(conf.Host)
	var executor exec.Executor = exec.NewECMSExecutorForHost(ecmsV1Alpha1.Exec())

	if err := ecmsV1Alpha1.Files().Create(ctx, global.ClusterDataPath, true, nil); err != nil {
		return err
	}
	user, err := user.Current()
	if err != nil {
		return err
	}

	// 初始化自定义proton sysctl 内核参数
	n.Logger.Info(fmt.Sprintf("update node %s proton sysctl", conf.Host))
	if err := n.UpdateProtonSysctlFile(executor, ecmsV1Alpha1.Files()); err != nil {
		return fmt.Errorf("update remote host %s proton sysctl file failed: %w", conf.Host, err)
	}

	// 加载内核模块
	n.Logger.Info(fmt.Sprintf("update node %s kernel modules", conf.Host))
	if err := n.UpdateKernelModules(n.Logger, executor, ecmsV1Alpha1.Files()); err != nil {
		return fmt.Errorf("update remote host %s kernel modules failed: %w", conf.Host, err)
	}

	// k8s 配置更新 放在slb更新前更新否则无法连接apiserver
	n.Logger.Info(fmt.Sprintf("update node %s proton k8s", conf.Host))
	var kubeCluster *configuration.ClusterConfiguration
	if n.OldClusterConf != nil && len(n.OldClusterConf.Nodes) != 0 && n.isMaster(conf.HostName) {
		// k8s 配置变更
		if n.ClusterConf.Cs.Ha_port != n.OldClusterConf.Cs.Ha_port ||
			n.ClusterConf.Cs.Etcd_data_dir != n.OldClusterConf.Cs.Etcd_data_dir {
			_, clientSet := client.NewK8sClient()
			if clientSet == nil {
				return client.ErrKubernetesClientSetNil
			}
			ctx := context.Background()
			kubeCm, err := clientSet.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubeadm-config", metav1.GetOptions{})
			if err != nil {
				return err
			}
			kubeClusterConf := kubeCm.Data["ClusterConfiguration"]
			if err := yaml.Unmarshal([]byte(kubeClusterConf), &kubeCluster); err != nil {
				return err
			}
			// etcd data path change
			if n.ClusterConf.Cs.Etcd_data_dir != n.OldClusterConf.Cs.Etcd_data_dir {
				if err := ecmsV1Alpha1.Files().Create(ctx, n.ClusterConf.Cs.Etcd_data_dir, true, nil); err != nil {
					return err
				}
				dir, err := ecmsV1Alpha1.Files().ListDirectory(ctx, n.ClusterConf.Cs.Etcd_data_dir)
				if err != nil {
					return err
				}
				if len(dir) > 0 {
					return fmt.Errorf("etcd path [%s] is not empty", n.ClusterConf.Cs.Etcd_data_dir)
				}

				n.Logger.Debugf("etcd data path change on node: %s", conf.Host)
				kubeCluster.Etcd.Local.DataDir = n.ClusterConf.Cs.Etcd_data_dir
			}
			// ha change
			if n.ClusterConf.Cs.Ha_port != n.OldClusterConf.Cs.Ha_port {
				n.Logger.Debugf("proton cs ha change on node: %s", conf.Host)
				kubeCluster.ControlPlaneEndpoint = fmt.Sprintf("proton-cs.lb.aishu.cn:%d", n.ClusterConf.Cs.Ha_port)
			}

			// cr ha change
			if n.ClusterConf.Cr.Local != nil && n.ClusterConf.Cr.Local.Ha_ports.Registry != n.OldClusterConf.Cr.Local.Ha_ports.Registry {
				n.Logger.Debugf("proton cr ha change on node: %s", conf.Host)
				kubeCluster.ImageRepository = fmt.Sprintf(`registry.aishu.cn:%d/public`, n.ClusterConf.Cr.Local.Ha_ports.Registry)
			}

			kubeClusterBytes, err := yaml.Marshal(kubeCluster)
			if err != nil {
				return err
			}
			// 在第一个master上修改configmap
			if conf.HostName == n.ClusterConf.Cs.Master[0] {

				kubeCm.Data["ClusterConfiguration"] = string(kubeClusterBytes)
				patchDataBytes, err := json.Marshal(kubeCm)
				if err != nil {
					return err
				}
				_, err = clientSet.CoreV1().ConfigMaps("kube-system").Patch(ctx, "kubeadm-config", types.MergePatchType, patchDataBytes, metav1.PatchOptions{})
				if err != nil {
					return err
				}
			}

			//迁移etcd 数据
			if n.ClusterConf.Cs.Etcd_data_dir != n.OldClusterConf.Cs.Etcd_data_dir {

				if n.isMaster(conf.HostName) {
					// 备份etcd yaml
					cmd := "mv /{etc/kubernetes/manifests,tmp}/etcd.yaml"
					n.Logger.Debugf("backup etcd manifest on node: %s", conf.Host)
					if err := executor.Command("bash", "-c", cmd).Run(); err != nil {
						return err
					}
					// 等待etcd停止
					cmd = "while [[ -n $(crictl pods --name=etcd --namespace=kube-system --quiet --state=ready) ]]; do sleep 1; done"
					n.Logger.Debug("waiting etcd stop")
					n.Logger.Debugf("run cmd [%s] on node: %s", cmd, conf.Host)
					if err := executor.Command("bash", "-c", cmd).Run(); err != nil {
						return err
					}
					// 迁移数据
					cmd = fmt.Sprintf("if [[ -d %s ]]; then cp --recursive --target-directory=%s %s/*; fi",
						n.OldClusterConf.Cs.Etcd_data_dir,
						n.ClusterConf.Cs.Etcd_data_dir,
						n.OldClusterConf.Cs.Etcd_data_dir)
					n.Logger.Debugf("migrate etcd data on node: %s", conf.Host)
					n.Logger.Debugf("run cmd [%s] on node: %s", cmd, conf.Host)
					if err := executor.Command("bash", "-c", cmd).Run(); err != nil {
						return err
					}
					if err := ecmsV1Alpha1.Files().Delete(ctx, "/tmp/kubeadm-config-etcd.yaml"); err != nil {
						return err
					}
					if err := ecmsV1Alpha1.Files().Create(ctx, "/tmp/kubeadm-config-etcd.yaml", false, kubeClusterBytes); err != nil {
						return err
					}
					cmd = "kubeadm init phase etcd local --config=/tmp/kubeadm-config-etcd.yaml"
					n.Logger.Debugf("kubeadm init etcd on node: %s", conf.Host)
					n.Logger.Debugf("run cmd [%s] on node: %s", cmd, conf.Host)
					if err := executor.Command("bash", "-c", cmd).Run(); err != nil {
						return err
					}
				}
			}
		}
	}

	n.Logger.Debug(fmt.Sprintf("set hostname on node:%s", conf.Host))
	var curHostname string
	{
		out, err := executor.Command("hostname").Output()
		if err != nil {
			return err
		}
		curHostname = string(bytes.TrimSpace(out))
	}

	// set hostname
	hostname := configuration.GetNodeNameByIP(conf.Host, n.ClusterConf.Nodes)
	if curHostname != hostname {
		if err := executor.Command("hostnamectl", "set-hostname", hostname).Run(); err != nil {
			return err
		}
	}

	// internalDomainName
	n.Logger.Debug(fmt.Sprintf("set internalDomainName on node:%s", conf.Host))
	//Open没有Append模式，只能先读出来，再重写进去，之前的Create模式会把文件内历史配置清掉
	hosts, err := ecmsV1Alpha1.Files().ReadFile(ctx, global.HostsPath)
	if err != nil {
		return fmt.Errorf("unable to read file %s error: %w", global.HostsPath, err)
	}

	h := strings.Split(string(hosts), "\n")
	// ip hostname

	for i := 0; i < len(n.ClusterConf.Nodes); i++ {
		if n.ClusterConf.Nodes[i].IP4 != "" {
			h = n.SetHostsDomainName(h, configuration.GetNodeNameByIP(n.ClusterConf.Nodes[i].IP4, n.ClusterConf.Nodes), n.ClusterConf.Nodes[i].IP4)
		} else {
			h = n.SetHostsDomainName(h, configuration.GetNodeNameByIP(n.ClusterConf.Nodes[i].IP6, n.ClusterConf.Nodes), n.ClusterConf.Nodes[i].IP6)
		}
	}
	// nodeIP cr
	h = n.SetHostsDomainName(h, global.RegistryDomain, "127.0.0.1")
	h = n.SetHostsDomainName(h, global.ChartmuseumDomain, "127.0.0.1")
	h = n.SetHostsDomainName(h, global.RpmDomain, "127.0.0.1")

	// NodeIP cs
	h = n.SetHostsDomainName(h, global.ProtonCsDomain, "127.0.0.1")

	var hostByte []byte
	for i := 0; i < len(h); i++ {
		if len([]byte(h[i])) == 0 {
			continue
		}
		hostByte = append(hostByte, []byte(h[i]+"\n")...)
	}
	if err := ecmsV1Alpha1.Files().Create(ctx, global.HostsPath, false, hostByte); err != nil {
		return err
	}

	haConf := configuration.HaProxyConf{}
	if err := json.Unmarshal([]byte(configuration.HaDefaultConf), &haConf); err != nil {
		return err
	}
	haConf.Conf.CsBackend.Server = []string{}
	haConf.Conf.CsBackend.DefaultServer = "verify none check-ssl inter 3s downinter 5s rise 2 fall 2 slowstart 60s maxconn 5000 maxqueue 5000 weight 100"

	// cs ha
	for i := 0; i < len(n.ClusterConf.Cs.Master); i++ {
		NodeName := n.ClusterConf.Cs.Master[i]
		for j := 0; j < len(n.ClusterConf.Nodes); j++ {
			if NodeName == n.ClusterConf.Nodes[j].Name {
				var ha string
				if n.ClusterConf.Nodes[0].IP4 != "" {
					ha = n.ClusterConf.Nodes[j].Name + " " + n.ClusterConf.Nodes[j].IP4 + ":6443" + " check"
				} else {
					ha = n.ClusterConf.Nodes[j].Name + " [" + n.ClusterConf.Nodes[j].IP6 + "]:6443" + " check"
				}
				haConf.Conf.CsBackend.Server = append(haConf.Conf.CsBackend.Server, ha)
			}
		}
	}
	haConf.Conf.CsFrontend.Bind = fmt.Sprintf(":::%d v4v6", n.ClusterConf.Cs.Ha_port)
	// cr ha
	if n.ClusterConf.Cr.Local != nil {
		for i := 0; i < len(n.ClusterConf.Cr.Local.Hosts); i++ {
			NodeName := n.ClusterConf.Cr.Local.Hosts[i]
			for j := 0; j < len(n.ClusterConf.Nodes); j++ {
				if NodeName == n.ClusterConf.Nodes[j].Name {
					var ha string
					if n.ClusterConf.Nodes[0].IP4 != "" {
						ha = n.ClusterConf.Nodes[j].Name + " " + n.ClusterConf.Nodes[j].IP4
					} else {
						ha = n.ClusterConf.Nodes[j].Name + " [" + n.ClusterConf.Nodes[j].IP6 + "]"
					}
					haConf.Conf.CrChartmuseumBackend.Server = append(
						haConf.Conf.CrChartmuseumBackend.Server,
						fmt.Sprintf("%s:%d check", ha, n.ClusterConf.Cr.Local.Ports.Chartmuseum))
					haConf.Conf.CrRegistryBackend.Server = append(
						haConf.Conf.CrRegistryBackend.Server,
						fmt.Sprintf("%s:%d check", ha, n.ClusterConf.Cr.Local.Ports.Registry))
					haConf.Conf.CrRpmBackend.Server = append(
						haConf.Conf.CrRpmBackend.Server,
						fmt.Sprintf("%s:%d check", ha, n.ClusterConf.Cr.Local.Ports.Rpm))
				}
			}
		}
		haConf.Conf.CrChartmuseumFrontend.Bind = fmt.Sprintf(":::%d v4v6", n.ClusterConf.Cr.Local.Ha_ports.Chartmuseum)
		haConf.Conf.CrRegistryFrontend.Bind = fmt.Sprintf(":::%d v4v6", n.ClusterConf.Cr.Local.Ha_ports.Registry)
		haConf.Conf.CrRpmFrontend.Bind = fmt.Sprintf(":::%d v4v6", n.ClusterConf.Cr.Local.Ha_ports.Rpm)
	}

	haConfJSON, _ := json.Marshal(haConf)
	log.Printf("haConfJSON: %s", haConfJSON)

	remoteSLB := slbops.NewRemote(ecmsV1Alpha1)
	n.Logger.Debug("reconcile haproxy config")
	if err := remoteSLB.EnsureHAProxy(context.TODO(), haConf); err != nil {
		return fmt.Errorf("unable to reconcile haproxy config: %w", err)
	}

	// 为未开启安全检查的SLB HA配置开启安全检查
	if err := remoteSLB.EnsureKeepalivedScriptSecurity(context.TODO()); err != nil {
		n.Logger.Warning(fmt.Sprintf("Cannot update Proton SLB config to add script security:%s", conf.Host), err)
	}

	// 配置 Chrony，如果是内置master节点作为时间服务器的话，随机选择发生在校验配置文件那一步，这里只负责读取
	// 如果模式是UserManaged用户手动管理，则不进行任何变更
	if n.ClusterConf.Chrony.Mode != configuration.ChronyModeUserManaged {
		if n.ClusterConf.Chrony.Mode == configuration.ChronyModeLocalMaster && n.ClusterConf.Chrony.Server[0] == conf.HostName {
			// 如果模式是LocalMaster且本节点为被选中的时间服务器节点，则以空servers数组进行Chrony配置
			n.Logger.WithField("node", conf.HostName).Debug("This node will be acting as NTP server for other nodes in this cluster")
			if err := n.UpdateChronyConfig(n.Logger.WithField("node", conf.HostName), exec.NewECMSExecutorForHost(ecmsV1Alpha1.Exec()), ecmsV1Alpha1.Files(), []string{}); err != nil {
				return fmt.Errorf("update chrony config fail: %w", err)
			}
		} else if n.ClusterConf.Chrony.Mode == configuration.ChronyModeLocalMaster {
			// 如果模式是LocalMaster且本节点不是被选中的时间服务器节点，则用Chrony结构体中记录的hostname查出被选中节点的ip，用它作为唯一时间服务器
			n.Logger.WithField("node", conf.HostName).Debug("This node will receive time information from the selected master node")
			servers := []string{configuration.GetIPByNodeName(n.ClusterConf.Chrony.Server[0], n.ClusterConf.Nodes)}
			if err := n.UpdateChronyConfig(n.Logger.WithField("node", conf.HostName), exec.NewECMSExecutorForHost(ecmsV1Alpha1.Exec()), ecmsV1Alpha1.Files(), servers); err != nil {
				return fmt.Errorf("update chrony config fail: %w", err)
			}
		} else if n.ClusterConf.Chrony.Mode == configuration.ChronyModeExternalNTP {
			// 如果模式是ExternalNTP，则直接使用传入的服务器列表作为唯一服务器组
			n.Logger.WithField("node", conf.HostName).Debug("This node will receive time information from external NTP server(s)")
			if err := n.UpdateChronyConfig(n.Logger.WithField("node", conf.HostName), exec.NewECMSExecutorForHost(ecmsV1Alpha1.Exec()), ecmsV1Alpha1.Files(), n.ClusterConf.Chrony.Server); err != nil {
				return fmt.Errorf("update chrony config fail: %w", err)
			}
		}
	}

	// TODO：配置 helm repo 移动到 cr 模块
	if n.OldClusterConf != nil && len(n.OldClusterConf.Nodes) != 0 {
		// helm repo ha change 需要haproxy 更新之后更新
		if n.ClusterConf.Cr.Local != nil && n.ClusterConf.Cr.Local.Ha_ports.Chartmuseum != n.OldClusterConf.Cr.Local.Ha_ports.Chartmuseum {
			if conf.HostName == n.ClusterConf.Cs.Master[0] {
				host, username, password := global.Chartmuseum(n.ClusterConf.Cr)
				if host == "" {
					// helm repo add 命令及其参数
					var argv []string = []string{"helm", "repo", "add", global.HelmRepo, host}
					if username != "" {
						argv = append(argv, "--username")
						argv = append(argv, username)
						argv = append(argv, "--password")
						argv = append(argv, password)
					}
					cmd := strings.Join(argv, " ")
					n.Logger.Debug("upgrade helm repo")
					n.Logger.Debugf("run cmd [%s] on node: %s", cmd, conf.Host)
					if err := executor.Command("bash", "-c", cmd).Run(); err != nil {
						return err
					}
					// helm3 repo 需要添加仓库信息
					argv[0] = "helm3"
					cmd = strings.Join(argv, " ")
					n.Logger.Debugf("run cmd [%s] on node: %s", cmd, conf.Host)
					if err := executor.Command("bash", "-c", cmd).Run(); err != nil {
						return err
					}
				}
			}
		}

		// proton cs ha change  需要haproxy 更新之后更新
		if n.ClusterConf.Cs.Ha_port != n.OldClusterConf.Cs.Ha_port {
			kubeClusterConfByte, _ := yaml.Marshal(kubeCluster)
			if err := ecmsV1Alpha1.Files().Delete(ctx, "/tmp/kubeadm-config-change.yaml"); err != nil {
				return err
			}

			var delimiter = "---\n"
			kubeClusterConfByte = append(kubeClusterConfByte, []byte(delimiter)...)
			var kubeJoin *configuration.KubeadmJoinDefaultStruct
			var kubeInit *configuration.KubeadmInitDefaultStruct
			var kubelet *configuration.KubeadmKubeletDefaultStruct
			if err := yaml.Unmarshal([]byte(configuration.KubeadmJoinDefault), &kubeJoin); err != nil {
				return err
			}
			if err := yaml.Unmarshal([]byte(configuration.KubeadmInitDefault), &kubeInit); err != nil {
				return err
			}
			if err := yaml.Unmarshal([]byte(configuration.KubeadmKubeletDefault), &kubelet); err != nil {
				return err
			}
			kubeJoin.ControlPlane.LocalAPIEndpoint.AdvertiseAddress = conf.Host
			kubeInit.LocalAPIEndpoint.AdvertiseAddress = conf.Host
			kubeInit.NodeRegistration.KubeletExtraArgs.NodeIP = conf.Host
			kubeJoinByte, err := yaml.Marshal(kubeJoin)
			if err != nil {
				return err
			}
			kubeInitByte, err := yaml.Marshal(kubeInit)
			if err != nil {
				return err
			}
			kubeletByte, err := yaml.Marshal(kubelet)
			if err != nil {
				return err
			}
			kubeClusterConfByte = append(kubeClusterConfByte, kubeInitByte...)
			kubeClusterConfByte = append(kubeClusterConfByte, []byte(delimiter)...)
			kubeClusterConfByte = append(kubeClusterConfByte, kubeJoinByte...)
			kubeClusterConfByte = append(kubeClusterConfByte, []byte(delimiter)...)
			kubeClusterConfByte = append(kubeClusterConfByte, kubeletByte...)

			if err := ecmsV1Alpha1.Files().Create(ctx, "/tmp/kubeadm-config-change.yaml", false, kubeClusterConfByte); err != nil {
				return err
			}

			// 更新kubelet
			n.Logger.Debugf("set kubelet ha on node: %s", conf.Host)
			var kubeletConfigStruct *configuration.KubeConfig
			kubeletConfigByte, err := ecmsV1Alpha1.Files().ReadFile(ctx, "/etc/kubernetes/kubelet.conf")
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(kubeletConfigByte, &kubeletConfigStruct); err != nil {
				return err
			}
			if err := ecmsV1Alpha1.Files().Delete(ctx, "/etc/kubernetes/kubelet.conf"); err != nil {
				return err
			}
			kubeletConfigStruct.Clusters[0].Cluster.Server = fmt.Sprintf("https://proton-cs.lb.aishu.cn:%d", n.ClusterConf.Cs.Ha_port)
			kubeletConfigByte, err = yaml.Marshal(kubeletConfigStruct)
			if err != nil {
				return err
			}
			if err := ecmsV1Alpha1.Files().Create(ctx, "/etc/kubernetes/kubelet.conf", false, kubeletConfigByte); err != nil {
				return err
			}
			if err := executor.Command("systemctl", "restart", "kubelet").Run(); err != nil {
				return err
			}

			// master 节点更新
			if n.isMaster(conf.HostName) && n.isOldMaster(conf.HostName) {
				// admin
				n.Logger.Debugf("set admin ha on node: %s", conf.Host)
				var adminConfigStruct *configuration.KubeConfig
				adminConfigByte, err := ecmsV1Alpha1.Files().ReadFile(ctx, "/etc/kubernetes/admin.conf")
				if err != nil {
					return err
				}
				if err := yaml.Unmarshal(adminConfigByte, &adminConfigStruct); err != nil {
					return err
				}
				adminConfigStruct.Clusters[0].Cluster.Server = fmt.Sprintf("https://proton-cs.lb.aishu.cn:%d", n.ClusterConf.Cs.Ha_port)
				adminConfigByte, err = yaml.Marshal(adminConfigStruct)
				if err != nil {
					return err
				}
				if err := ecmsV1Alpha1.Files().Create(ctx, "/etc/kubernetes/admin.conf", false, adminConfigByte); err != nil {
					return err
				}

				// controller manager

				n.Logger.Debugf("set controller manager ha on node: %s", conf.Host)
				var controllerManagerConfigStruct *configuration.KubeConfig
				controllerManagerConfigByte, err := ecmsV1Alpha1.Files().ReadFile(ctx, "/etc/kubernetes/controller-manager.conf")
				if err != nil {
					return err
				}
				if err := yaml.Unmarshal(controllerManagerConfigByte, &controllerManagerConfigStruct); err != nil {
					return err
				}
				controllerManagerConfigStruct.Clusters[0].Cluster.Server = fmt.Sprintf("https://proton-cs.lb.aishu.cn:%d", n.ClusterConf.Cs.Ha_port)
				controllerManagerConfigByte, err = yaml.Marshal(controllerManagerConfigStruct)
				if err != nil {
					return err
				}
				if err := ecmsV1Alpha1.Files().Create(ctx, "/etc/kubernetes/controller-manager.conf", false, controllerManagerConfigByte); err != nil {
					return err
				}

				// scheduler

				n.Logger.Debugf("set scheduler ha on node: %s", conf.Host)
				var schedulerConfigStruct *configuration.KubeConfig
				schedulerConfigByte, err := ecmsV1Alpha1.Files().ReadFile(ctx, "/etc/kubernetes/scheduler.conf")
				if err != nil {
					return err
				}
				if err := yaml.Unmarshal(schedulerConfigByte, &schedulerConfigStruct); err != nil {
					return err
				}
				schedulerConfigStruct.Clusters[0].Cluster.Server = fmt.Sprintf("https://proton-cs.lb.aishu.cn:%d", n.ClusterConf.Cs.Ha_port)
				schedulerConfigByte, err = yaml.Marshal(schedulerConfigStruct)
				if err != nil {
					n.Logger.Error(err)
				}
				if err := ecmsV1Alpha1.Files().Create(ctx, "/etc/kubernetes/scheduler.conf", false, schedulerConfigByte); err != nil {
					return err
				}

				if conf.HostName == n.ClusterConf.Cs.Master[0] {
					n.Logger.Debugf("run cmd [%s] on node: %s ", "kubeadm init phase upload-config all --config=/tmp/kubeadm-config-change.yaml", conf.Host)
					if err := executor.Command("kubeadm", "init", "phase", "upload-config", "all", "--config=/tmp/kubeadm-config-change.yaml").Run(); err != nil {
						return fmt.Errorf("upload config on node :%s err : %w", conf.Host, err)
					}

					n.Logger.Debugf("run cmd [%s] on node: %s ", "kubeadm init phase bootstrap-token --config=/tmp/kubeadm-config-change.yaml", conf.Host)
					if err := executor.Command("kubeadm", "init", "phase", "bootstrap-token", "--config=/tmp/kubeadm-config-change.yaml").Run(); err != nil {
						return fmt.Errorf("bootstrap token on node :%s err : %s", conf.Host, err)
					}

					n.Logger.Debugf("run cmd [%s] on node: %s ", "kubeadm init phase addon kube-proxy --config=/tmp/kubeadm-config-change.yaml", conf.Host)
					if err := executor.Command("kubeadm", "init", "phase", "addon", "kube-proxy", "--config=/tmp/kubeadm-config-change.yaml").Run(); err != nil {
						return fmt.Errorf("update kube-proxy  on node :%s err : %s", conf.Host, err)
					}
				}
				if n.isMaster(conf.HostName) {
					if err := ecmsV1Alpha1.Files().Delete(ctx, fmt.Sprintf("%s/.kube/config", user.HomeDir)); err != nil {
						return err
					}
					if err := executor.Command("cp", "/etc/kubernetes/admin.conf", "/root/.kube/config").Run(); err != nil {
						return err
					}
				}
				if conf.HostName == n.ClusterConf.Cs.Master[0] {
					n.Logger.Debug("restart kube-proxy")
					time.Sleep(time.Second)
					if err := executor.Command("kubectl", "rollout", "restart", "daemonset/kube-proxy", "--namespace=kube-system").Run(); err != nil {
						return fmt.Errorf("restart kube-proxy on node :%s err : %s", conf.Host, err)
					}
				}
			}

			//	// create master
			//	if n.isMaster(conf.HostName) && !n.isOldMaster(conf.HostName){
			//		n.Logger.Debugf("node %s create master",conf.Host)
			//		kubeadmSession,_ := sshClient.NewSession()
			//		err = kubeadmSession.Run("kubeadm join --config=/tmp/kubeadm-config-change.yaml --skip-phases=preflight,kubelet-start")
			//		if err != nil {
			//			n.Logger.Error(err)
			//		}
			//		kubeadmSession.Close()
			//		sftpClient.Remove(fmt.Sprintf("%s/.kube/config",user.HomeDir))
			//		sftpClient.MkdirAll(fmt.Sprintf("%s/.kube",user.HomeDir))
			//		cpSession,_ := sshClient.NewSession()
			//		err = cpSession.Run(fmt.Sprintf("cp /etc/kubernetes/admin.conf %s/.kube/config",user.HomeDir))
			//		if err != nil {
			//			n.Logger.Error(err)
			//		}
			//		cpSession.Close()
			//		uid,_ := strconv.Atoi(user.Uid)
			//		gid ,_ := strconv.Atoi(user.Gid)
			//		sftpClient.Chown(fmt.Sprintf("%s/.kube/config",user.HomeDir),uid,gid)
			//
			//		helmSession, _ := sshClient.NewSession()
			//		err = helmSession.Run("helm init --client-only --skip-refresh")
			//		if err != nil {
			//			n.Logger.Error(err)
			//		}
			//		helmSession.Close()
			//
			//		helmSession, _ = sshClient.NewSession()
			//		err = helmSession.Run("helm repo remove stable")
			//		if err != nil {
			//			n.Logger.Error(err)
			//		}
			//		helmSession.Close()
			//
			//		helmSession, _ = sshClient.NewSession()
			//		err = helmSession.Run("helm repo remove local")
			//		if err != nil {
			//			n.Logger.Error(err)
			//		}
			//		helmSession.Close()
			//	}
			//
			//	// delete master
			//
			//	if !n.isMaster(conf.HostName) && n.isOldMaster(conf.HostName){
			//		n.Logger.Debugf("node %s delete master",conf.Host)
			//		kubeadmSession,_ := sshClient.NewSession()
			//		err = kubeadmSession.Run("kubeadm reset --skip-phases=preflight,cleanup-node")
			//		if err != nil {
			//			n.Logger.Error(err)
			//		}
			//		kubeadmSession.Close()
			//		// remove config
			//		sftpClient.Remove("/etc/kubernetes/admin.conf")
			//		sftpClient.Remove("/etc/kubernetes/controller-manager.conf")
			//		sftpClient.Remove("/etc/kubernetes/scheduler.conf")
			//
			//		sftpClient.RemoveDirectory(fmt.Sprintf("%s/.kube",user.HomeDir))
			//		//remove static pod
			//		sftpClient.Remove("/etc/kubernetes/manifests/etcd.yaml")
			//		sftpClient.Remove("/etc/kubernetes/manifests/kube-apiserver.yaml")
			//		sftpClient.Remove("/etc/kubernetes/manifests/kube-controller-manager.yaml")
			//		sftpClient.Remove("/etc/kubernetes/manifests/kube-scheduler.yaml")
			//		// remvoce cert
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver.crt")
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver.key")
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver-etcd-client.crt")
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver-etcd-client.key")
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver-kubelet-client.crt")
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver-kubelet-client.key")
			//		sftpClient.Remove("/etc/kubernetes/pki/ca.key")
			//		sftpClient.Remove("/etc/kubernetes/pki/apiserver.crt")
			//		sftpClient.RemoveDirectory("/etc/kubernetes/pki/etcd")
			//		sftpClient.Remove("/etc/kubernetes/pki/front-proxy-ca.crt")
			//		sftpClient.Remove("/etc/kubernetes/pki/front-proxy-ca.key")
			//		sftpClient.Remove("/etc/kubernetes/pki/front-proxy-client.crt")
			//		sftpClient.Remove("/etc/kubernetes/pki/front-proxy-client.key")
			//		sftpClient.Remove("/etc/kubernetes/pki/sa.pub")
			//		sftpClient.Remove("/etc/kubernetes/pki/sa.key")
			//	}
		}
	}
	return nil
}

func (n *Node) FindHostsDomainName(hosts []string, dstDomainName, dstIP string) bool {

	for i := range hosts {
		h := strings.Split(hosts[i], " ")
		if len(h) < 2 {
			continue
		}
		ip := h[0]
		domainName := h[1]

		if ip == dstIP && domainName == dstDomainName {
			return true
		}
	}

	return false

}

func (n *Node) SetHostsDomainName(hosts []string, dstDomainName, dstIP string) []string {

	isAppend := false
	for i := 0; i < len(hosts); i++ {
		h := strings.Split(hosts[i], " ")
		if len(h) != 2 {
			continue
		}
		ip := h[0]
		domainName := h[1]

		if domainName == dstDomainName && ip == dstIP {
			isAppend = true
			break
		} else if domainName == dstDomainName && ip != dstIP {
			hosts[i] = dstIP + " " + dstDomainName
			isAppend = true
			break
		}
	}

	if !isAppend {
		hosts = append(hosts, dstIP+" "+dstDomainName)
	}
	return hosts
}

// RemoveConf 移除 proton-cli 在各个节点保存的配置文件，忽略错误 ErrNotExist
func (n *Node) RemoveConf() error {
	for _, node := range n.ClusterConf.Nodes {
		n.Logger.Printf("remove old proton cli dir of %v", node)
		if err := configuration.RemoveOldProtonCLIDirIfExist(ecms.NewForHost(node.IP()).Files()); err != nil {
			return fmt.Errorf("remove old proton cli dir of %v fail: %w", node, err)
		}
	}
	return nil
}

func (n *Node) isMaster(nodeName string) bool {
	var master bool
	for i := 0; i < len(n.ClusterConf.Cs.Master); i++ {
		if n.ClusterConf.Cs.Master[i] == nodeName {
			master = true
			break
		}
	}
	return master
}

func (n *Node) isOldMaster(nodeName string) bool {
	var master bool
	if n.OldClusterConf != nil && len(n.OldClusterConf.Nodes) != 0 {
		for i := 0; i < len(n.OldClusterConf.Cs.Master); i++ {
			if n.OldClusterConf.Cs.Master[i] == nodeName {
				master = true
				break
			}
		}
	}
	return master
}

// 重置防火墙
func (n *Node) resetFirewall(c *configuration.Firewall, f firewalld.Interface) error {
	if c.Mode != configuration.FirewallFirewalld {
		n.Logger.WithField("mode", c.Mode).Debug("skipped reset firewall")
		return nil
	}

	// 清理持久化配置
	if err := resetFirewalldZoneInterfaces(f, true, firewalld.ZoneProtonCS); err != nil {
		return err
	}
	if err := resetFirewalldZoneSources(f, true, firewalld.ZoneProtonCS); err != nil {
		return err
	}

	active, err := f.State()
	if !active || err != nil {
		return err
	}

	// 清理运行时配置
	if err := resetFirewalldZoneInterfaces(f, false, firewalld.ZoneProtonCS); err != nil {
		return err
	}
	if err := resetFirewalldZoneSources(f, false, firewalld.ZoneProtonCS); err != nil {
		return err
	}

	return nil
}

func resetFirewalldZoneInterfaces(c firewalld.Interface, p bool, z string) error {
	got, err := c.ListZoneInterfaces(p, z)
	if err != nil {
		return err
	}

	if len(got) == 0 {
		return nil
	}

	return c.RemoveZoneInterfaces(p, z, got)
}

func resetFirewalldZoneSources(c firewalld.Interface, p bool, z string) error {
	got, err := c.ListZoneSources(p, z)
	if err != nil {
		return err
	}

	if len(got) == 0 {
		return nil
	}

	return c.RemoveZoneSources(p, z, got)
}
