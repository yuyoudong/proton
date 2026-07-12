package app

import (
	"fmt"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration"
)

// BuildHelmValues 将 ClusterConfig 转换为 helm install/upgrade 所需的 values map，
// 等价于 deploy.sh 中 -f ~/.kweaver-ai/config.yaml 所传入的内容。
//
// 生成的 map 结构与 deploy/conf/config.yaml 保持一致，包含：
//
//	namespace, env, image, accessAddress, storage, depServices
func BuildHelmValues(cfg *configuration.ClusterConfig, namespace string) map[string]any {
	v := make(map[string]any)

	// namespace
	ns := namespace
	if ns == "" && cfg.Deploy != nil && cfg.Deploy.Namespace != "" {
		ns = cfg.Deploy.Namespace
	}
	if ns == "" {
		ns = "openbkn"
	}
	v["namespace"] = ns

	// env
	envMap := map[string]any{
		"language": "en_US.UTF-8",
		"timezone": "Asia/Shanghai",
	}
	if cfg.Env != nil {
		if cfg.Env.Language != "" {
			envMap["language"] = cfg.Env.Language
		}
		if cfg.Env.Timezone != "" {
			envMap["timezone"] = cfg.Env.Timezone
		}
	}
	v["env"] = envMap

	// image.registry - 优先使用内置 CR 仓库
	imageRegistry := ""
	if cfg.Cr != nil {
		if cfg.Cr.Local != nil && len(cfg.Cr.Local.Hosts) > 0 {
			// 使用内置 CR 仓库：registry.aishu.cn:15000 或 node1:5000
			host := cfg.Cr.Local.Hosts[0]
			port := cfg.Cr.Local.Ports.Registry
			if port == 0 {
				port = 5000
			}
			// 优先使用 HA 端口（15000）
			if cfg.Cr.Local.Ha_ports.Registry != 0 {
				port = cfg.Cr.Local.Ha_ports.Registry
				// HA 端口通常使用 registry.aishu.cn 域名
				if host != "" {
					host = "registry.aishu.cn"
				}
			}
			imageRegistry = fmt.Sprintf("%s:%d", host, port)
		} else if cfg.Cr.External != nil {
			// 使用外部 CR 仓库
			if cfg.Cr.External.Registry != nil && cfg.Cr.External.Registry.Host != "" {
				imageRegistry = cfg.Cr.External.Registry.Host
			} else if cfg.Cr.External.OCI != nil && cfg.Cr.External.OCI.Registry != "" {
				imageRegistry = cfg.Cr.External.OCI.Registry
			}
		}
	}
	if imageRegistry != "" {
		v["image"] = map[string]any{
			"registry": imageRegistry,
		}
	}

	// accessAddress
	if cfg.AccessAddress != nil {
		aa := map[string]any{}
		if cfg.AccessAddress.Host != "" {
			aa["host"] = cfg.AccessAddress.Host
		}
		if cfg.AccessAddress.Port != 0 {
			aa["port"] = cfg.AccessAddress.Port
		}
		if cfg.AccessAddress.Scheme != "" {
			aa["scheme"] = cfg.AccessAddress.Scheme
		} else {
			aa["scheme"] = "https"
		}
		if cfg.AccessAddress.Path != "" {
			aa["path"] = cfg.AccessAddress.Path
		} else {
			aa["path"] = "/"
		}
		v["accessAddress"] = aa
	}

	// storage
	if cfg.Storage != nil && cfg.Storage.StorageClassName != "" {
		v["storage"] = map[string]any{
			"storageClassName": cfg.Storage.StorageClassName,
		}
	}

	// depServices
	dep := make(map[string]any)
	if cfg.ResourceConnectInfo != nil {
		dep = buildDepServices(cfg.ResourceConnectInfo)
	}

	// zookeeper 从 ClusterConfig 顶层 ZooKeeper 字段获取
	if cfg.ZooKeeper != nil && len(cfg.ZooKeeper.Hosts) > 0 {
		zkHost := "zookeeper-headless.resource.svc.cluster.local"
		zkPort := 2181
		dep["zookeeper"] = map[string]any{
			"host": zkHost,
			"port": zkPort,
		}
	}

	// class-443 默认使用 class-443 ingressClass
	if _, ok := dep["class-443"]; !ok {
		dep["class-443"] = map[string]any{
			"ingressClass": "class-443",
		}
	}

	v["depServices"] = dep

	return v
}

// buildDepServices 将 ResourceConnectInfo 转换为 depServices map，
// 字段名称和层级与 deploy/conf/config.yaml 中 depServices 保持一致。
func buildDepServices(r *configuration.ResourceConnectInfo) map[string]any {
	dep := make(map[string]any)

	// rds
	if r.Rds != nil {
		rds := map[string]any{}
		if r.Rds.Hosts != "" {
			rds["host"] = r.Rds.Hosts
		}
		if r.Rds.HostsRead != "" {
			rds["hostRead"] = r.Rds.HostsRead
		}
		if r.Rds.Port != 0 {
			rds["port"] = r.Rds.Port
		}
		if r.Rds.PortRead != 0 {
			rds["portRead"] = r.Rds.PortRead
		}
		if r.Rds.Username != "" {
			rds["user"] = r.Rds.Username
		}
		if r.Rds.Password != "" {
			rds["password"] = r.Rds.Password
		}
		if r.Rds.RdsType != "" {
			rds["type"] = string(r.Rds.RdsType)
		}
		if r.Rds.AdminKey != "" {
			rds["admin_key"] = r.Rds.AdminKey
		}
		if r.Rds.SourceType != "" {
			rds["source_type"] = string(r.Rds.SourceType)
		}
		// database 默认值，charts 通常期望此字段存在
		if _, ok := rds["database"]; !ok {
			rds["database"] = "openbkn"
		}
		dep["rds"] = rds
	}

	// redis
	if r.Redis != nil {
		redis := map[string]any{}
		if r.Redis.ConnectType != "" {
			redis["connectType"] = string(r.Redis.ConnectType)
		}
		if r.Redis.SourceType != "" {
			redis["sourceType"] = string(r.Redis.SourceType)
		}
		connectInfo := map[string]any{}
		if r.Redis.MasterGroupName != "" {
			connectInfo["masterGroupName"] = r.Redis.MasterGroupName
		}
		if r.Redis.Username != "" {
			connectInfo["username"] = r.Redis.Username
		}
		if r.Redis.Password != "" {
			connectInfo["password"] = r.Redis.Password
		}
		if r.Redis.SentinelHosts != "" {
			connectInfo["sentinelHost"] = r.Redis.SentinelHosts
		}
		if r.Redis.SentinelPort != 0 {
			connectInfo["sentinelPort"] = r.Redis.SentinelPort
		}
		if r.Redis.SentinelUsername != "" {
			connectInfo["sentinelUsername"] = r.Redis.SentinelUsername
		}
		if r.Redis.SentinelPassword != "" {
			connectInfo["sentinelPassword"] = r.Redis.SentinelPassword
		}
		if r.Redis.MasterHosts != "" {
			connectInfo["masterHost"] = r.Redis.MasterHosts
		}
		if r.Redis.MasterPort != 0 {
			connectInfo["masterPort"] = r.Redis.MasterPort
		}
		if r.Redis.Hosts != "" {
			connectInfo["host"] = r.Redis.Hosts
		}
		if r.Redis.Port != 0 {
			connectInfo["port"] = r.Redis.Port
		}
		if len(connectInfo) > 0 {
			redis["connectInfo"] = connectInfo
		}
		dep["redis"] = redis
	}

	// mq (kafka)
	if r.Mq != nil {
		mq := map[string]any{}
		if r.Mq.MqType != "" {
			mq["mqType"] = string(r.Mq.MqType)
		}

		if r.Mq.MqHosts != "" {
			mq["mqHost"] = r.Mq.MqHosts
		}
		if r.Mq.MqPort != 0 {
			mq["mqPort"] = r.Mq.MqPort
		}

		// mqLookupdHost 和 mqLookupdPort 必须存在（即使为空）
		if r.Mq.MqLookupdHosts != "" {
			mq["mqLookupdHost"] = r.Mq.MqLookupdHosts
		} else {
			mq["mqLookupdHost"] = ""
		}
		if r.Mq.MqLookupdPort != 0 {
			mq["mqLookupdPort"] = r.Mq.MqLookupdPort
		} else {
			mq["mqLookupdPort"] = 0
		}
		if r.Mq.Auth != nil {
			auth := map[string]any{}
			if r.Mq.Auth.Mechanism != "" {
				auth["mechanism"] = string(r.Mq.Auth.Mechanism)
			}
			if r.Mq.Auth.Username != "" {
				auth["username"] = r.Mq.Auth.Username
			}
			if r.Mq.Auth.Password != "" {
				auth["password"] = r.Mq.Auth.Password
			}
			if len(auth) > 0 {
				mq["auth"] = auth
			}
		}
		dep["mq"] = mq
	}

	// opensearch
	if r.OpenSearch != nil {
		os_ := map[string]any{}
		if r.OpenSearch.Hosts != "" {
			os_["host"] = r.OpenSearch.Hosts
		}
		if r.OpenSearch.Port != 0 {
			os_["port"] = r.OpenSearch.Port
		}
		if r.OpenSearch.Username != "" {
			os_["user"] = r.OpenSearch.Username
		}
		if r.OpenSearch.Password != "" {
			os_["password"] = r.OpenSearch.Password
		}
		if r.OpenSearch.Protocol != "" {
			os_["protocol"] = r.OpenSearch.Protocol
		}
		if r.OpenSearch.Distribution != "" {
			os_["distribution"] = r.OpenSearch.Distribution
		}
		if r.OpenSearch.Version != "" {
			os_["version"] = string(r.OpenSearch.Version)
		}
		if r.OpenSearch.SourceType != "" {
			os_["sourceType"] = string(r.OpenSearch.SourceType)
		}
		dep["opensearch"] = os_
	}

	// mongodb
	if r.Mongodb != nil {
		mongo := map[string]any{}
		if r.Mongodb.Hosts != "" {
			mongo["host"] = r.Mongodb.Hosts
		}
		if r.Mongodb.Port != 0 {
			mongo["port"] = r.Mongodb.Port
		}
		if r.Mongodb.Username != "" {
			mongo["user"] = r.Mongodb.Username
		}
		if r.Mongodb.Password != "" {
			mongo["password"] = r.Mongodb.Password
		}
		if r.Mongodb.ReplicaSet != "" {
			mongo["replicaSet"] = r.Mongodb.ReplicaSet
		}
		if r.Mongodb.AuthSource != "" {
			mongo["options"] = map[string]any{
				"authSource": r.Mongodb.AuthSource,
			}
		}
		if r.Mongodb.SourceType != "" {
			mongo["source_type"] = string(r.Mongodb.SourceType)
		}
		dep["mongodb"] = mongo
	}

	// zookeeper（从 ClusterConfig.ZooKeeper 传入时需另外处理，此处留空）
	// 如需支持 ZooKeeper 连接信息，可在 ResourceConnectInfo 中扩展

	return dep
}
