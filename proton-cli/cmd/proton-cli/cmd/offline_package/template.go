package offline_package

import (
	"fmt"
	"strings"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// architectureInfo 保存同一架构在不同分发介质中的命名形式。
type architectureInfo struct {
	goArch      string
	packageArch string
}

// normalizeArchitecture 将用户输入的架构标识转换为模板生成所需的标准命名。
func normalizeArchitecture(arch string) (architectureInfo, error) {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64":
		return architectureInfo{
			goArch:      "amd64",
			packageArch: "x86_64",
		}, nil
	case "arm64", "aarch64":
		return architectureInfo{
			goArch:      "arm64",
			packageArch: "aarch64",
		}, nil
	default:
		return architectureInfo{}, fmt.Errorf("unsupported architecture %q, supported values: amd64, arm64", arch)
	}
}

// renderManifestTemplate 根据目标架构生成离线包 manifest 的 YAML 模板。
// protonCLIPath specifies the local path to proton-cli binary.
// protonCLIVersion specifies the version of proton-cli to download.
// These two parameters are mutually exclusive - if protonCLIVersion is set (and not empty),
// it takes precedence over protonCLIPath.
func renderManifestTemplate(arch string, protonCLIPath string, protonCLIVersion string) ([]byte, error) {
	info, err := normalizeArchitecture(arch)
	if err != nil {
		return nil, err
	}

	m := defaultManifest(info, protonCLIPath, protonCLIVersion)

	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest template: %w", err)
	}

	return out, nil
}

// defaultManifest 返回指定架构下的默认离线包目标清单。
// protonCLIPath specifies the local path to proton-cli binary.
// protonCLIVersion specifies the version of proton-cli to download.
func defaultManifest(info architectureInfo, protonCLIPath string, protonCLIVersion string) *Manifest {
	// Build proton-cli artifact based on parameters
	// protonCLIVersion takes precedence over protonCLIPath
	var protonCLIArtifact Artifact
	if protonCLIVersion != "" {
		protonCLIArtifact = httpArtifact(
			"proton-cli",
			"https://github.com/kweaver-ai/proton/releases/download/v"+protonCLIVersion+"/proton-cli-linux-"+info.goArch+".tar.gz",
			"tar+gzip",
			"proton-cli/bin/proton-cli",
		)
	} else if protonCLIPath != "" {
		protonCLIArtifact = Artifact{
			Name: "proton-cli",
			Source: Source{
				File: &FileSource{
					Path: protonCLIPath,
				},
			},
		}
	}

	// Build binaries list
	binaries := []Artifact{}
	// Add proton-cli if configured
	if protonCLIVersion != "" || protonCLIPath != "" {
		binaries = append(binaries, protonCLIArtifact)
	}
	// Add other binaries
	binaries = append(binaries,
		httpArtifact("calicoctl", "https://github.com/projectcalico/calico/releases/download/v3.25.2/calicoctl-linux-"+info.goArch, "", ""),
		httpArtifact("helm", "https://get.helm.sh/helm-v3.20.0-linux-"+info.goArch+".tar.gz", "tar+gzip", "linux-"+info.goArch+"/helm"),
		httpArtifact("nerdctl", "https://github.com/containerd/nerdctl/releases/download/v2.2.1/nerdctl-2.2.1-linux-"+info.goArch+".tar.gz", "tar+gzip", "nerdctl"),
		httpArtifact("skopeo", "https://github.com/kweaver-ai/proton/releases/download/skopeo-1.19.0/skopeo-linux-"+info.goArch, "", ""),
	)

	return &Manifest{
		TypeMeta: meta.TypeMeta{
			APIVersion: "proton.kweaver.ai/v1alpha1",
			Kind:       "Manifest",
		},
		ObjectMeta: meta.ObjectMeta{
			Name: "example-manifest",
		},
		Spec: ManifestSpec{
			Architecture: info.goArch,
			Binaries:     binaries,
			Charts: []Artifact{
				httpArtifact("component-manage-0.0.3.tgz", "https://github.com/kweaver-ai/proton/releases/download/component-manage-0.0.3/component-manage-0.0.3.tgz", "", ""),
				httpArtifact("ingress-nginx-4.15.0.tgz", "https://github.com/kweaver-ai/proton/releases/download/ingress-nginx-4.15.0/ingress-nginx-4.15.0.tgz", "", ""),
				httpArtifact("nvidia-device-plugin-1.0.0-0.14.5.tgz", "https://github.com/kweaver-ai/proton/releases/download/nvidia-device-plugin-1.0.0-0.14.5/nvidia-device-plugin-1.0.0-0.14.5.tgz", "", ""),
				httpArtifact("proton-grafana-1.0.6.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-grafana-1.0.6/proton-grafana-1.0.6.tgz", "", ""),
				httpArtifact("proton-kafka-5.6.0.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-kafka-5.6.0/proton-kafka-5.6.0.tgz", "", ""),
				httpArtifact("proton-kube-state-metrics-2.4.3.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-kube-state-metrics-2.4.3/proton-kube-state-metrics-2.4.3.tgz", "", ""),
				httpArtifact("proton-node-exporter-1.6.2.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-node-exporter-1.6.2/proton-node-exporter-1.6.2.tgz", "", ""),
				httpArtifact("proton-opensearch-1.8.8.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-opensearch-1.8.8/proton-opensearch-1.8.8.tgz", "", ""),
				httpArtifact("proton-prometheus-1.0.1.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-prometheus-1.0.1/proton-prometheus-1.0.1.tgz", "", ""),
				httpArtifact("proton-redis-1.11.2.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-redis-1.11.2/proton-redis-1.11.2.tgz", "", ""),
				httpArtifact("proton-zookeeper-5.6.0.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-zookeeper-5.6.0/proton-zookeeper-5.6.0.tgz", "", ""),
				httpArtifact("rds-mariadb-operator-0.0.1.tgz", "https://github.com/kweaver-ai/proton/releases/download/proton-mariadb-0.0.1/rds-mariadb-operator-0.0.1.tgz", "", ""),
			},
			Images: []Artifact{
				ociArtifact("calico-cni:v3.25.2", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/quay.io/calico/cni:v3.25.2"),
				ociArtifact("calico-kube-controllers:v3.25.2", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/quay.io/calico/kube-controllers:v3.25.2"),
				ociArtifact("calico-node:v3.25.2", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/quay.io/calico/node:v3.25.2"),
				ociArtifact("coredns:v1.12.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/coredns/coredns:v1.12.1"),
				ociArtifact("etcd:3.6.5-0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/etcd:3.6.5-0"),
				ociArtifact("etcd:v3.3.19", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/etcd:v3.3.19"),
				ociArtifact("ingress-nginx-controller:v1.15.0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/ingress-nginx/controller:v1.15.0"),
				ociArtifact("ingress-nginx-kube-webhook-certgen:v1.6.8", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/ingress-nginx/kube-webhook-certgen:v1.6.8"),
				ociArtifact("kube-apiserver:v1.34.6", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/kube-apiserver:v1.34.6"),
				ociArtifact("kube-controller-manager:v1.34.6", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/kube-controller-manager:v1.34.6"),
				ociArtifact("kube-proxy:v1.34.6", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/kube-proxy:v1.34.6"),
				ociArtifact("kube-rbac-proxy:v0.11.0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/quay.io/brancz/kube-rbac-proxy:v0.11.0"),
				ociArtifact("kube-scheduler:v1.34.6", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/kube-scheduler:v1.34.6"),
				ociArtifact("kweaver-ai/proton/component-manage:0.0.3", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/component-manage:0.0.3"),
				ociArtifact("kweaver-ai/proton/rds-etcd:0.0.2", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/rds-etcd:0.0.2"),
				ociArtifact("kweaver-ai/proton/rds-exporter:0.0.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/rds-exporter:0.0.1"),
				ociArtifact("kweaver-ai/proton/rds-mariadb:0.0.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/rds-mariadb:0.0.1"),
				ociArtifact("kweaver-ai/proton/rds-mgmt:0.0.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/rds-mgmt:0.0.1"),
				ociArtifact("kweaver-ai/proton/rds-operator-controller:0.0.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/rds-operator-controller:0.0.1"),
				ociArtifact("kweaver-ai/proton/rds-operator-proxy:0.0.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/rds-operator-proxy:0.0.1"),
				ociArtifact("pause:3.10.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/registry.k8s.io/pause:3.10.1"),
				ociArtifact("proton/kube-state-metrics:v2.4.2", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/proton/kube-state-metrics:v2.4.2"),
				ociArtifact("proton/node-exporter:v1.5.0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/proton/node-exporter:v1.5.0"),
				ociArtifact("proton/proton-grafana:1.0.6-20250707.2.90ad92", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-grafana:1.0.6-20250707.2.90ad92"),
				ociArtifact("proton/proton-kafka:5.6.0-20251110.2.f1ef4340", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-kafka:5.6.0-20251110.2.f1ef4340"),
				ociArtifact("proton/proton-opensearch-exporter:1.8.8", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-opensearch-exporter:1.8.8"),
				ociArtifact("proton/proton-opensearch:1.8.8", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-opensearch:1.8.8"),
				ociArtifact("proton/proton-redis:1.11.2-20251029.2.169ac3c0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-redis:1.11.2-20251029.2.169ac3c0"),
				ociArtifact("proton/proton-zookeeper-exporter:5.6.0-20250625.2.138fb9", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-zookeeper-exporter:5.6.0-20250625.2.138fb9"),
				ociArtifact("proton/proton-zookeeper:5.6.0-20250625.2.138fb9", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/kweaver-ai/proton/proton-zookeeper:5.6.0-20250625.2.138fb9"),
				ociArtifact("public/busybox:1.36.1", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/docker.io/library/busybox:1.36.1"),
				ociArtifact("public/kafka-exporter:v1.6.0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/docker.io/danielqsj/kafka-exporter:v1.6.0"),
				ociArtifact("public/oliver006/redis_exporter:v1.77.0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/quay.io/oliver006/redis_exporter:v1.77.0"),
				ociArtifact("public/prometheus-config-reloader:v0.66.0", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/ghcr.io/prometheus-operator/prometheus-config-reloader:v0.66.0"),
				ociArtifact("public/prometheus:v2.37.8", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/mirror/quay.io/prometheus/prometheus:v2.37.8"),
			},
			RPMs: []Artifact{
				// Example: Pull RPM from OCI registry using oras
				// ociArtifact("example-package."+info.packageArch+".rpm", "registry.example.com/rpms/example-package:1.0.0-"+info.packageArch),
				ociArtifact("containerd-2.2.2-3.proton."+info.packageArch+".rpm", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/proton/rpm/containerd:2.2.2-3.proton."+info.packageArch),
				httpArtifact("cri-tools-1.34.0-150500.1.1."+info.packageArch+".rpm", "https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.34/rpm/"+info.packageArch+"/cri-tools-1.34.0-150500.1.1."+info.packageArch+".rpm", "", ""),
				ociArtifact("ecms-1.1.8-120.1.el7."+info.packageArch+".rpm", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/proton/rpm/ecms:1.1.8-120.1.proton."+info.packageArch),
				httpArtifact("haproxy-2.5.6."+info.packageArch+".rpm", "https://github.com/kweaver-ai/proton/releases/download/haproxy%2F2.5.6/haproxy-2.5.6."+info.packageArch+".rpm", "", ""),
				httpArtifact("kubeadm-1.34.6-150500.1.1."+info.packageArch+".rpm", "https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.34/rpm/"+info.packageArch+"/kubeadm-1.34.6-150500.1.1."+info.packageArch+".rpm", "", ""),
				httpArtifact("kubectl-1.34.6-150500.1.1."+info.packageArch+".rpm", "https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.34/rpm/"+info.packageArch+"/kubectl-1.34.6-150500.1.1."+info.packageArch+".rpm", "", ""),
				httpArtifact("kubelet-1.34.6-150500.1.1."+info.packageArch+".rpm", "https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.34/rpm/"+info.packageArch+"/kubelet-1.34.6-150500.1.1."+info.packageArch+".rpm", "", ""),
				httpArtifact("kubernetes-cni-1.7.1-150500.1.1."+info.packageArch+".rpm", "https://mirrors.aliyun.com/kubernetes-new/core/stable/v1.34/rpm/"+info.packageArch+"/kubernetes-cni-1.7.1-150500.1.1."+info.packageArch+".rpm", "", ""),
				httpArtifact("proton-cr-1.2.5-87.el7."+info.packageArch+".rpm", "https://github.com/kweaver-ai/proton/releases/download/proton-cr-1.2.5/proton-cr-1.2.5-87.el7."+info.packageArch+".rpm", "", ""),
				httpArtifact("proton-cr-chartmuseum-0.15.0."+info.packageArch+".rpm", "https://github.com/kweaver-ai/proton/releases/download/proton-cr-chartmuseum-0.15.0/proton-cr-chartmuseum-0.15.0."+info.packageArch+".rpm", "", ""),
				httpArtifact("proton-cr-registry-2.7.1."+info.packageArch+".rpm", "https://github.com/kweaver-ai/proton/releases/download/proton-cr-registry-2.7.1/proton-cr-registry-2.7.1."+info.packageArch+".rpm", "", ""),
				ociArtifact("runc-1.4.2-3.proton."+info.packageArch+".rpm", "swr.cn-east-3.myhuaweicloud.com/kweaver-ai/proton/rpm/runc:1.4.2-3.proton."+info.packageArch),
			},
		},
	}
}

// httpArtifact 构造一个基于 HTTP 下载的制品定义。
func httpArtifact(name, url, format, path string) Artifact {
	return Artifact{
		Name: name,
		Source: Source{
			HTTP: &HTTPSource{
				URL:    url,
				Format: format,
				Path:   path,
			},
		},
	}
}

// ociArtifact 构造一个基于 OCI 引用的制品定义。
func ociArtifact(name, reference string) Artifact {
	return Artifact{
		Name: name,
		Source: Source{
			OCI: &OCISource{
				Reference: reference,
			},
		},
	}
}
