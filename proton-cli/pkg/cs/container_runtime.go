package cs

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"k8s.io/apimachinery/pkg/util/sets"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/core/global"
	k "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/cs/kubernetes"
)

// isSpecifiedContainerRuntimeSource 返回是否指定了容器运行时
func isSpecifiedContainerRuntimeSource(s *configuration.ContainerRuntimeSource) bool {
	return s.Containerd != nil
}

// 用于标识节点已拥有的容器运行时
type nodeContainerRuntime string

const (
	// containerd
	nodeContainerRuntimeContainerd nodeContainerRuntime = "containerd"
)

func runtimePackageNames(r nodeContainerRuntime) []string {
	switch r {
	case nodeContainerRuntimeContainerd:
		return []string{"containerd", "containerd.io"}
	default:
		return nil
	}
}

// detectNodeCommonContainerRuntime 探查所有节点共有的容器运行时
func detectNodeCommonContainerRuntime(kc *k.KubernetesCluster) (nodeContainerRuntime, error) {
	// found container runtimes
	found := sets.New[nodeContainerRuntime]()

	for _, n := range append(kc.Workers, kc.Masters...) {
		runtimes, err := detectNodeContainerRuntimes(&n)
		if err != nil {
			return "", err
		}
		found.Insert(runtimes...)
	}

	if found.Has(nodeContainerRuntimeContainerd) {
		return nodeContainerRuntimeContainerd, nil
	}

	return "", errors.New("container runtime not found")
}

func detectNodeContainerRuntimes(n *k.Node) (runtimes []nodeContainerRuntime, err error) {
	for _, pkg := range runtimePackageNames(nodeContainerRuntimeContainerd) {
		if _, err := n.Query(pkg); err != nil {
			continue
		}
		runtimes = append(runtimes, nodeContainerRuntimeContainerd)
		break
	}
	return
}

func generateContainerRuntimeSourceInto(r nodeContainerRuntime, target *configuration.ContainerRuntimeSource, localCR *configuration.LocalCR, containerdRoot string) {
	switch r {
	case nodeContainerRuntimeContainerd:
		// 如果 target.Containerd 已存在，则补充缺失的字段；否则创建新的
		if target.Containerd == nil {
			target.Containerd = generateContainerdContainerRuntimeSource(localCR, containerdRoot)
		} else {
			fillContainerdContainerRuntimeSource(target.Containerd, localCR)
		}
	default:
		return
	}
}

// fillContainerdContainerRuntimeSource 补充 containerd 配置中缺失的字段
func fillContainerdContainerRuntimeSource(target *configuration.ContainerdContainerRuntimeSource, localCR *configuration.LocalCR) {
	if localCR == nil {
		return
	}

	// 补充缺失的 SandboxImage
	if target.SandboxImage == "" {
		target.SandboxImage = fmt.Sprintf("%s/pause:3.10.1", net.JoinHostPort(global.RegistryDomain, strconv.Itoa(localCR.Ha_ports.Registry)))
	}

	// 补充缺失的 Registries
	if len(target.Registries) == 0 {
		var hosts []string
		hosts = append(hosts, net.JoinHostPort(global.RegistryDomain, strconv.Itoa(localCR.Ha_ports.Registry)))
		for _, h := range localCR.Hosts {
			hosts = append(hosts, net.JoinHostPort(h, strconv.Itoa(localCR.Ports.Registry)))
		}
		for _, h := range hosts {
			target.Registries = append(target.Registries, generateContainerdRegistryHostConfig(h))
		}
	}
}

func generateContainerdContainerRuntimeSource(localCR *configuration.LocalCR, root string) *configuration.ContainerdContainerRuntimeSource {
	if root == "" {
		root = "/var/lib/containerd"
	}
	s := &configuration.ContainerdContainerRuntimeSource{
		Root: root,
		// TODO: generate structurally
		SandboxImage: fmt.Sprintf("%s/pause:3.10.1", net.JoinHostPort(global.RegistryDomain, strconv.Itoa(localCR.Ha_ports.Registry))),
	}

	var hosts []string
	hosts = append(hosts, net.JoinHostPort(global.RegistryDomain, strconv.Itoa(localCR.Ha_ports.Registry)))
	for _, h := range localCR.Hosts {
		hosts = append(hosts, net.JoinHostPort(h, strconv.Itoa(localCR.Ports.Registry)))
	}

	for _, h := range hosts {
		s.Registries = append(s.Registries, generateContainerdRegistryHostConfig(h))
	}

	return s
}

func generateContainerdRegistryHostConfig(host string) configuration.RegistryHostConfig {
	s := &url.URL{
		// registry.aishu.cn:15000 和各个 node:5000 的 registry 使用 http 协议
		Scheme: "http",
		Host:   host,
	}
	skipVerify := true
	return configuration.RegistryHostConfig{
		Server: s.String(),
		HostConfigs: map[string]configuration.RegistryHostFileConfig{
			host: {
				SkipVerify: &skipVerify,
			},
		},
	}
}


