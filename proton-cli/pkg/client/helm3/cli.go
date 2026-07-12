package helm3

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	helmcli "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/release"
)

type Client interface {
	Install(release string, chartRef *ChartRef, opts ...InstallOption) error
	Upgrade(release string, chartRef *ChartRef, opts ...UpgradeOption) error
	Uninstall(release string, opts ...UninstallOption) error

	// NameSpace 切换命名空间
	NameSpace(ns string) Client

	GetRelease(name string) (*release.Release, error)
	PullChart(name, version string, reg *OCIRegistryConfig) (string, func(), error)
	PushChart(f string, reg *OCIRegistryConfig) error
}

type (
	L = []any
	M = map[string]any
)

type helmv3 struct {
	actionConfig *action.Configuration
	settings     *helmcli.EnvSettings
	namespace    string
	log          logrus.FieldLogger
}

func NewCli(namespace string, log *logrus.Entry) (Client, error) {
	if log == nil {
		_log := logrus.New()
		_log.SetLevel(logrus.DebugLevel)
		log = _log.WithField("helm", "v3")
	}

	settings := helmcli.New()
	actionConfig := new(action.Configuration)

	err := actionConfig.Init(settings.RESTClientGetter(), namespace, "", log.Debugf)
	if err != nil {
		log.WithError(err).Errorln("action config init failed")
		return nil, fmt.Errorf("helm3 action config init failed: %w", err)
	}

	return &helmv3{
		actionConfig: actionConfig,
		settings:     settings,
		namespace:    namespace,
		log:          log,
	}, nil
}

func (c *helmv3) NameSpace(ns string) Client {
	if c.namespace == ns {
		return c
	}
	// 创建新的 settings 并设置 namespace，确保 Helm 所有操作使用正确的 namespace
	newSettings := helmcli.New()
	newSettings.SetNamespace(ns)
	nsActionConfig := new(action.Configuration)
	_ = nsActionConfig.Init(newSettings.RESTClientGetter(), ns, "", c.log.Debugf)
	// helmDriver 为 sql 时才有可能出现错误
	return &helmv3{
		actionConfig: nsActionConfig,
		settings:     newSettings,
		namespace:    ns,
		log:          c.log,
	}
}
