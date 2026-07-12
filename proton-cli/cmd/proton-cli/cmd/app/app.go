package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/cmd/proton-cli/cmd/offline_package"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration/completion"
	protonapp "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/proton/app"
)

var appCmd = &cobra.Command{
	Use:   "app",
	Short: "Manage application lifecycle: import, export, install, and uninstall",
	Long: `Manage KWeaver application lifecycle operations.

Complete workflow:
  1. Export offline package:  proton-cli app export -f manifest.yaml -o package.tar
  2. Import to cluster:       proton-cli app import package.tar
  3. Install application:     proton-cli app install -f manifest.yaml
  4. Uninstall application:   proton-cli app uninstall -f manifest.yaml

Available commands:
  import     Import offline application package to built-in registry and ChartMuseum
  export     Export application package from registries for offline deployment
  install    Install application from manifest (with dependencies)
  uninstall  Uninstall application from manifest (with dependencies)`,
}

type appInstallFlags struct {
	manifestFile    string
	namespace       string
	timeout         string
	dryRun          bool
	createNamespace bool
	helmRepoName    string
	helmRepoURL     string
	configFile      string
	imageRegistry   string
	accessAddress   string
	accessHost      string
	accessPort      int
	accessPortSet   bool
	accessScheme    string
	accessSchemeSet bool
	setValues       []string
}

func newAppInstallCmd() *cobra.Command {
	f := &appInstallFlags{
		timeout:         "30m",
		createNamespace: true,
	}

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install a product from a VersionSet manifest",
		Long: `Install a KWeaver product by reading a VersionSet manifest file.
The manifest declares dependencies and helm releases with optional stage ordering.

Examples:
  proton-cli app install -f ./release-manifests/0.4.0/kweaver-dip.yaml
  proton-cli app install -f ./release-manifests/0.4.0/kweaver-dip.yaml -n kweaver --timeout 60m
  proton-cli app install -f ./release-manifests/0.4.0/kweaver-dip.yaml --dry-run
  proton-cli app install -f ./release-manifests/0.4.0/kweaver-dip.yaml --config ~/.kweaver-ai/config.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			f.accessPortSet = cmd.Flags().Changed("access-port")
			f.accessSchemeSet = cmd.Flags().Changed("access-scheme")
			return runAppInstall(cmd.Context(), f)
		},
	}

	cmd.Flags().StringVarP(&f.manifestFile, "file", "f", "", "path to VersionSet manifest YAML (required)")
	cmd.Flags().StringVarP(&f.namespace, "namespace", "n", "openbkn", "target Kubernetes namespace")
	cmd.Flags().StringVar(&f.timeout, "timeout", f.timeout, "per-release install timeout (e.g. 30m, 1h)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print install plan without executing")
	cmd.Flags().BoolVar(&f.createNamespace, "create-namespace", f.createNamespace, "create namespace if it does not exist")
	cmd.Flags().StringVar(&f.helmRepoName, "helm-repo-name", "", "override helm repo name for all releases")
	cmd.Flags().StringVar(&f.helmRepoURL, "helm-repo-url", "", "override helm repo URL for all releases (adds/updates repo before install)")
	cmd.Flags().StringVar(&f.configFile, "config", "", "path to deploy config.yaml (fallback if K8s secret unavailable)")
	cmd.Flags().StringVar(&f.imageRegistry, "image-registry", "", "container image registry (e.g. swr.cn-east-3.myhuaweicloud.com/kweaver-ai)")
	cmd.Flags().StringVar(&f.accessAddress, "access-address", "", "cluster access address URL (e.g. https://1.1.1.1:8443/)")
	cmd.Flags().StringVar(&f.accessHost, "access-host", "", "cluster access address host (auto-detect from K8s nodes if not specified)")
	cmd.Flags().IntVar(&f.accessPort, "access-port", 443, "cluster access address port")
	cmd.Flags().StringVar(&f.accessScheme, "access-scheme", "https", "cluster access address scheme (http/https)")
	cmd.Flags().StringSliceVar(&f.setValues, "set", nil, "set install values (repeatable key=value, supports dotted keys)")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func runAppInstall(ctx context.Context, f *appInstallFlags) error {
	if f.manifestFile == "" {
		return fmt.Errorf("manifest file is required (-f)")
	}

	if _, err := os.Stat(f.manifestFile); err != nil {
		return fmt.Errorf("manifest file %q: %w", f.manifestFile, err)
	}

	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true, TimestampFormat: "15:04:05"})

	// Build helm values: try K8s secret first, then config file fallback
	var values map[string]any

	cfg, k8sErr := loadClusterConfig(f.namespace)
	if k8sErr == nil && cfg.ResourceConnectInfo != nil {
		log.Info("Loaded depServices from K8s cluster config (proton-cli get conf)")
		values = protonapp.BuildHelmValues(cfg, f.namespace)
	} else if f.configFile != "" {
		log.Infof("Loading values from config file: %s", f.configFile)
		var err error
		values, err = loadConfigFileAsValues(f.configFile)
		if err != nil {
			return fmt.Errorf("load config file %s: %w", f.configFile, err)
		}
	} else {
		log.Warn("No depServices available: K8s secret not found and no --config specified")
		log.Warn("Charts may fail if they require depServices values")
		values = make(map[string]any)
	}

	// Apply image registry override or default
	if f.imageRegistry != "" {
		values["image"] = map[string]any{"registry": f.imageRegistry}
	} else if _, ok := values["image"]; !ok {
		// Default registry for community edition
		values["image"] = map[string]any{"registry": "swr.cn-east-3.myhuaweicloud.com/kweaver-ai"}
	}

	// Apply accessAddress override or auto-detect
	if f.accessPortSet || f.accessSchemeSet || f.accessHost != "" || f.accessAddress != "" {
		log.Info("Applying CLI accessAddress override")
	}
	existingAddr, _ := values["accessAddress"].(map[string]any)
	accessAddr, err := resolveAccessAddress(existingAddr, f, detectAccessHost)
	if err != nil {
		return fmt.Errorf("resolve access address: %w", err)
	}
	values["accessAddress"] = accessAddr

	setValues, err := parseAppInstallSet(f.setValues)
	if err != nil {
		return fmt.Errorf("parse --set values: %w", err)
	}
	values = protonapp.DeepMergeValues(values, setValues)

	// 确定 Helm 仓库地址（优先级：CLI 参数 > ClusterConfig 内置仓库 > manifest）
	helmRepoName := f.helmRepoName
	helmRepoURL := f.helmRepoURL

	// 如果 CLI 未指定，尝试从 ClusterConfig 获取内置 ChartMuseum 配置
	if helmRepoURL == "" && cfg != nil && cfg.Cr != nil && cfg.Cr.Local != nil {
		if len(cfg.Cr.Local.Hosts) > 0 {
			host := cfg.Cr.Local.Hosts[0]
			port := cfg.Cr.Local.Ports.Chartmuseum
			if port == 0 {
				port = 5001
			}
			// 优先使用 HA 端口（15001）
			if cfg.Cr.Local.Ha_ports.Chartmuseum != 0 {
				port = cfg.Cr.Local.Ha_ports.Chartmuseum
				// HA 端口通常使用 chartmuseum.aishu.cn 域名
				if host != "" {
					host = "chartmuseum.aishu.cn"
				}
			}
			helmRepoURL = fmt.Sprintf("http://%s:%d", host, port)
			// 使用标准的内置仓库名 helm_repos（与 proton-cli apply 创建的仓库一致）
			if helmRepoName == "" {
				helmRepoName = "helm_repos"
			}
			log.Infof("Using built-in ChartMuseum: %s (repo: %s)", helmRepoURL, helmRepoName)
		}
	}

	mgr, err := protonapp.NewManager(f.namespace, log)
	if err != nil {
		return fmt.Errorf("init app manager: %w", err)
	}

	opts := protonapp.InstallOptions{
		Namespace:       f.namespace,
		Timeout:         f.timeout,
		DryRun:          f.dryRun,
		CreateNamespace: f.createNamespace,
		HelmRepoName:    helmRepoName,
		HelmRepoURL:     helmRepoURL,
		SetValues:       setValues,
	}

	if err := mgr.InstallWithValues(ctx, f.manifestFile, values, opts); err != nil {
		return err
	}
	log.Info(appInstallCompletedMessage(f.manifestFile, f.namespace))
	return nil
}

func appInstallCompletedMessage(manifestFile, namespace string) string {
	return fmt.Sprintf("App install completed successfully: manifest=%s namespace=%s", manifestFile, namespace)
}

// loadClusterConfig 从 K8s Secret 加载 ClusterConfig，失败时返回空配置（不报错），
// 以兼容未通过 proton-cli apply 初始化的外部 K8s 集群。
// 注意：proton-cli-config secret 所在的 namespace 与安装目标 namespace 无关，
// LoadFromKubernetes 会从 ~/.proton-cli.yaml 中读取正确的 secret namespace。
func loadClusterConfig(installNamespace string) (*configuration.ClusterConfig, error) {
	_, k := client.NewK8sClient()
	if k == nil {
		logrus.Warn("kubernetes client unavailable, using empty cluster config (no dep services values)")
		return &configuration.ClusterConfig{}, nil
	}

	// 不传 namespace，让 LoadFromKubernetes 自行从 ~/.proton-cli.yaml 获取 secret namespace
	cfg, err := configuration.LoadFromKubernetes(context.Background(), k)
	if err != nil {
		logrus.Warnf("load cluster config from default namespace failed (%v), trying common namespaces...", err)
		// 依次尝试常见 namespace
		for _, ns := range []string{"proton", "anyshare", installNamespace} {
			if ns == "" {
				continue
			}
			cfg, err = configuration.LoadFromKubernetes(context.Background(), k, ns)
			if err == nil {
				logrus.Infof("Found proton-cli-config in namespace %q", ns)
				break
			}
		}
		if err != nil {
			return &configuration.ClusterConfig{}, fmt.Errorf("proton-cli-config secret not found")
		}
	}

	// 补全旧集群的 ResourceConnectInfo
	if cfg.ResourceConnectInfo == nil {
		if err := completion.CompleteOldClusterConfFromSecret(cfg, k); err != nil {
			logrus.Warnf("complete old cluster conf: %v", err)
		}
	}

	return cfg, nil
}

// ensureHelmRepo 确保指定 helm repo 已注册（add or update）。
func ensureHelmRepo(name, url string) error {
	// 使用 helm SDK 的 repo add 逻辑
	// 这里直接执行 helm repo add，与现有 deploy.sh 行为一致
	// TODO: 替换为 helm3 SDK 直接调用，避免 shell 依赖
	logrus.Infof("Ensuring helm repo %s -> %s", name, url)
	return nil
}

// detectAccessHost 从 K8s 集群节点自动探测访问地址。
// 优先返回第一个 master 节点的 InternalIP 或 ExternalIP。
func detectAccessHost() string {
	_, k := client.NewK8sClient()
	if k == nil {
		return ""
	}

	nodes, err := k.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return ""
	}

	// 优先选择 master 节点
	for _, node := range nodes.Items {
		if _, isMaster := node.Labels["node-role.kubernetes.io/master"]; isMaster {
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
					return addr.Address
				}
			}
		}
	}

	// 如果没有 master 节点，选择第一个节点
	for _, addr := range nodes.Items[0].Status.Addresses {
		if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
			return addr.Address
		}
	}

	return ""
}

func parseAccessAddressURL(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse access address url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("access address scheme must be http or https")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("access address host is required")
	}
	if u.RawQuery != "" {
		return nil, fmt.Errorf("access address query is not supported")
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("access address fragment is not supported")
	}
	if u.User != nil {
		return nil, fmt.Errorf("access address userinfo is not supported")
	}

	port := 0
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return nil, fmt.Errorf("parse access address port: %w", err)
		}
	} else if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}

	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}

	return map[string]any{
		"host":   u.Hostname(),
		"port":   port,
		"scheme": u.Scheme,
		"path":   path,
	}, nil
}

func resolveAccessAddress(existing map[string]any, f *appInstallFlags, detectHost func() string) (map[string]any, error) {
	if f != nil && f.accessAddress != "" {
		return parseAccessAddressURL(f.accessAddress)
	}

	resolved := normalizeAccessAddress(existing)

	if f != nil {
		if f.accessHost != "" {
			resolved["host"] = f.accessHost
		}
		if f.accessPortSet {
			resolved["port"] = f.accessPort
		}
		if f.accessSchemeSet {
			resolved["scheme"] = f.accessScheme
		}
	}

	if stringValue(resolved["host"]) == "" {
		if detectHost != nil {
			if host := detectHost(); host != "" {
				resolved["host"] = host
			}
		}
		if stringValue(resolved["host"]) == "" {
			resolved["host"] = "localhost"
		}
	}
	if intValue(resolved["port"]) == 0 {
		if stringValue(resolved["scheme"]) == "http" {
			resolved["port"] = 80
		} else {
			resolved["port"] = 443
		}
	}
	if stringValue(resolved["scheme"]) == "" {
		resolved["scheme"] = "https"
	}
	if stringValue(resolved["path"]) == "" {
		resolved["path"] = "/"
	}

	return resolved, nil
}

func normalizeAccessAddress(existing map[string]any) map[string]any {
	resolved := map[string]any{}
	if existing == nil {
		return resolved
	}
	if host := stringValue(existing["host"]); host != "" {
		resolved["host"] = host
	}
	if port := intValue(existing["port"]); port != 0 {
		resolved["port"] = port
	}
	if scheme := stringValue(existing["scheme"]); scheme != "" {
		resolved["scheme"] = scheme
	}
	if path := stringValue(existing["path"]); path != "" {
		resolved["path"] = path
	}
	return resolved
}

func parseAppInstallSet(entries []string) (map[string]any, error) {
	result := make(map[string]any)
	for _, entry := range entries {
		key, rawValue, ok := splitKeyValue(entry)
		if !ok {
			return nil, fmt.Errorf("invalid --set entry %q: expected key=value", entry)
		}

		parts, err := parseDottedKey(key)
		if err != nil {
			return nil, fmt.Errorf("invalid --set entry %q: %w", entry, err)
		}

		cursor := result
		for _, part := range parts[:len(parts)-1] {
			existing, ok := cursor[part]
			if !ok {
				next := make(map[string]any)
				cursor[part] = next
				cursor = next
				continue
			}
			next, ok := existing.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid --set entry %q: key %q conflicts with scalar value", entry, part)
			}
			cursor = next
		}
		cursor[parts[len(parts)-1]] = parseSetScalar(rawValue)
	}

	return result, nil
}

func splitKeyValue(entry string) (string, string, bool) {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return "", "", false
}

func parseDottedKey(key string) ([]string, error) {
	if key == "" {
		return nil, fmt.Errorf("key is required")
	}
	parts := make([]string, 0, 4)
	start := 0
	for i := 0; i <= len(key); i++ {
		if i != len(key) && key[i] != '.' {
			continue
		}
		part := key[start:i]
		if part == "" {
			return nil, fmt.Errorf("key %q contains an empty path segment", key)
		}
		parts = append(parts, part)
		start = i + 1
	}
	return parts, nil
}

func parseSetScalar(raw string) any {
	switch raw {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	return raw
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func intValue(v any) int {
	switch value := v.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

// loadConfigFileAsValues 将 deploy 格式的 config.yaml 直接加载为 map[string]interface{}，
// 可直接作为 helm values 使用（与 deploy.sh 中 -f config.yaml 效果一致）。
func loadConfigFileAsValues(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	if values == nil {
		values = make(map[string]any)
	}
	return values, nil
}

type appUninstallFlags struct {
	manifestFile string
	namespace    string
	timeout      string
	dryRun       bool
}

func newAppUninstallCmd() *cobra.Command {
	f := &appUninstallFlags{}

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall a product from a VersionSet manifest",
		Long: `Uninstall a KWeaver product by reading a VersionSet manifest file.
All helm releases defined in the manifest (including dependencies) will be uninstalled.
Uninstall order is reversed: current product first, then dependencies.`,
		Example: `  # Uninstall kweaver-dip and its dependencies
  proton-cli app uninstall -f kweaver-dip.yaml -n kweaver

  # Dry-run to see what would be uninstalled
  proton-cli app uninstall -f kweaver-dip.yaml -n kweaver --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppUninstall(cmd.Context(), f)
		},
	}

	cmd.Flags().StringVarP(&f.manifestFile, "file", "f", "", "Path to VersionSet manifest file (required)")
	cmd.Flags().StringVarP(&f.namespace, "namespace", "n", "openbkn", "Kubernetes namespace")
	cmd.Flags().StringVar(&f.timeout, "timeout", "5m", "per-release uninstall timeout (e.g. 5m, 10m)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print uninstall plan without executing")

	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func runAppUninstall(ctx context.Context, f *appUninstallFlags) error {
	if f.manifestFile == "" {
		return fmt.Errorf("manifest file is required (-f)")
	}

	if _, err := os.Stat(f.manifestFile); err != nil {
		return fmt.Errorf("manifest file %q: %w", f.manifestFile, err)
	}

	log := logrus.New()
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true, TimestampFormat: "15:04:05"})

	mgr, err := protonapp.NewManager(f.namespace, log)
	if err != nil {
		return fmt.Errorf("init app manager: %w", err)
	}

	opts := protonapp.UninstallOptions{
		Namespace: f.namespace,
		DryRun:    f.dryRun,
		Timeout:   f.timeout,
	}

	return mgr.Uninstall(ctx, f.manifestFile, opts)
}

func newAppImportCmd() *cobra.Command {
	// 直接使用 offline_package 的实现，作为别名
	offlineAppCmd := offline_package.NewCommand()

	// 找到 app import 子命令
	var importCmd *cobra.Command
	for _, subCmd := range offlineAppCmd.Commands() {
		if subCmd.Name() == "app" {
			for _, appSubCmd := range subCmd.Commands() {
				if appSubCmd.Name() == "import" {
					importCmd = appSubCmd
					break
				}
			}
			break
		}
	}

	if importCmd == nil {
		// Fallback: 创建一个简单的命令
		return &cobra.Command{
			Use:   "import <package.tar>",
			Short: "Import offline application package to built-in registry",
			Long: `Import an offline application package to the cluster's built-in registry and ChartMuseum.

The package contains:
  - Container images (pushed to built-in registry)
  - Helm charts (pushed to built-in ChartMuseum)
  - Manifest files (for app install)

After import, use 'proton-cli app install' to deploy the application.`,
			Example: `  # Import offline package (auto-detect built-in registry)
  proton-cli app import offline-app-package.tar --auto

  # Import with explicit registry and ChartMuseum
  proton-cli app import package.tar \
    --registry registry.aishu.cn:15000 \
    --chartmuseum-url http://chartmuseum.aishu.cn:15001

  # Force overwrite existing charts
  proton-cli app import package.tar --auto --force`,
			RunE: func(cmd *cobra.Command, args []string) error {
				return fmt.Errorf("app import: offline_package command not found")
			},
		}
	}

	// 更新帮助信息，保持与 app 命令风格一致
	importCmd.Short = "Import offline application package to built-in registry"
	importCmd.Long = `Import an offline application package to the cluster's built-in registry and ChartMuseum.

The package contains:
  - Container images (pushed to built-in registry)
  - Helm charts (pushed to built-in ChartMuseum)
  - Manifest files (for app install)

After import, use 'proton-cli app install' to deploy the application.`
	importCmd.Example = `  # Import offline package (auto-detect built-in registry)
  proton-cli app import offline-app-package.tar --auto

  # Import with explicit registry and ChartMuseum
  proton-cli app import package.tar \
    --registry registry.aishu.cn:15000 \
    --chartmuseum-url http://chartmuseum.aishu.cn:15001

  # Force overwrite existing charts
  proton-cli app import package.tar --auto --force`

	// 修改 Use 字段，移除 -i 参数要求，改为位置参数
	importCmd.Use = "import <package.tar>"
	importCmd.Args = cobra.MinimumNArgs(0) // 保持原有验证

	return importCmd
}

func newAppExportCmd() *cobra.Command {
	// 直接使用 offline_package 的实现，作为别名
	offlineAppCmd := offline_package.NewCommand()

	// 找到 app export 子命令
	var exportCmd *cobra.Command
	for _, subCmd := range offlineAppCmd.Commands() {
		if subCmd.Name() == "app" {
			for _, appSubCmd := range subCmd.Commands() {
				if appSubCmd.Name() == "export" {
					exportCmd = appSubCmd
					break
				}
			}
			break
		}
	}

	if exportCmd == nil {
		// Fallback: 创建一个简单的命令
		return &cobra.Command{
			Use:   "export",
			Short: "Export application package for offline deployment",
			Long: `Export an application package from registries for offline deployment.

The exported package contains:
  - Container images (pulled from registries)
  - Helm charts (pulled from Helm repositories)
  - Manifest files (VersionSet definitions)

The package can be imported to an offline cluster using 'proton-cli app import'.`,
			Example: `  # Export kweaver-dip application
  proton-cli app export -f kweaver-dip.yaml -o kweaver-dip-package.tar

  # Export for ARM64 platform
  proton-cli app export -f manifest.yaml --platform linux/arm64

  # Export without dependencies (only root manifest)
  proton-cli app export -f manifest.yaml --disable-dependencies

  # Continue on missing images
  proton-cli app export -f manifest.yaml --ignore-missing-images`,
			RunE: func(cmd *cobra.Command, args []string) error {
				return fmt.Errorf("app export: offline_package command not found")
			},
		}
	}

	// 更新帮助信息，保持与 app 命令风格一致
	exportCmd.Short = "Export application package for offline deployment"
	exportCmd.Long = `Export an application package from registries for offline deployment.

The exported package contains:
  - Container images (pulled from registries)
  - Helm charts (pulled from Helm repositories)
  - Manifest files (VersionSet definitions)

The package can be imported to an offline cluster using 'proton-cli app import'.

Output filename:
  If -o is not specified, the filename is auto-generated from the manifest:
  <product>-<version>-offline-package.tar (e.g., kweaver-dip-0.5.0-offline-package.tar)`
	exportCmd.Example = `  # Export kweaver-dip application (auto-generated filename)
  proton-cli app export -f kweaver-dip.yaml
  # Output: kweaver-dip-0.5.0-offline-package.tar

  # Export with custom filename
  proton-cli app export -f kweaver-dip.yaml -o my-package.tar

  # Export for ARM64 platform
  proton-cli app export -f manifest.yaml --platform linux/arm64

  # Export without dependencies (only root manifest)
  proton-cli app export -f manifest.yaml --disable-dependencies

  # Continue on missing images
  proton-cli app export -f manifest.yaml --ignore-missing-images`

	return exportCmd
}

func init() {
	appCmd.AddCommand(newAppImportCmd())
	appCmd.AddCommand(newAppExportCmd())
	appCmd.AddCommand(newAppInstallCmd())
	appCmd.AddCommand(newAppUninstallCmd())
}

func NewAppCommand() *cobra.Command {
	return appCmd
}
