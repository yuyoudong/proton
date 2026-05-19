package node

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/ecms/v1alpha1/files"
	exec "devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/client/exec/v1alpha1"
)

const (
	// protonModulesLoadPath 内核模块持久化配置路径，确保重启后自动加载
	protonModulesLoadPath = "/etc/modules-load.d/proton.conf"
)

// kernelVersion 解析后的内核版本号
type kernelVersion struct {
	Major int // 主版本号
	Minor int // 次版本号
	Patch int // 补丁版本号
}

// parseKernelVersion 解析 uname -r 输出，提取内核主版本号、次版本号和补丁版本号
func parseKernelVersion(uname string) (kernelVersion, error) {
	ver := kernelVersion{}
	parts := strings.Split(uname, "-")
	verParts := strings.Split(parts[0], ".")
	if len(verParts) < 2 {
		return ver, fmt.Errorf("unable to parse kernel version from: %s", uname)
	}
	var err error
	ver.Major, err = strconv.Atoi(verParts[0])
	if err != nil {
		return ver, fmt.Errorf("unable to parse major version: %w", err)
	}
	ver.Minor, err = strconv.Atoi(verParts[1])
	if err != nil {
		return ver, fmt.Errorf("unable to parse minor version: %w", err)
	}
	if len(verParts) >= 3 {
		ver.Patch, err = strconv.Atoi(verParts[2])
		if err != nil {
			return ver, fmt.Errorf("unable to parse patch version: %w", err)
		}
	}
	return ver, nil
}

// lessThan 判断内核版本是否低于指定的 major.minor
func (v kernelVersion) lessThan(major, minor int) bool {
	if v.Major != major {
		return v.Major < major
	}
	return v.Minor < minor
}

// getKernelModules 根据内核版本返回需要加载的内核模块列表
// 内核版本低于 4.19 时需要额外加载 nf_conntrack_ipv4 和 nf_conntrack_ipv6
func getKernelModules(ver kernelVersion) []string {
	modules := []string{
		"br_netfilter",
		"nf_conntrack",
	}

	if ver.lessThan(4, 19) {
		modules = append(modules, "nf_conntrack_ipv4", "nf_conntrack_ipv6")
	}

	return modules
}

// UpdateKernelModules 加载所需的内核模块并写入持久化配置
// 步骤：获取内核版本 -> 确定模块列表 -> 加载模块 -> 写入 /etc/modules-load.d/proton.conf
func (n *Node) UpdateKernelModules(logger logrus.FieldLogger, e exec.Executor, f files.Interface) error {
	var ctx = context.TODO()

	unameOut, err := e.Command("uname", "-r").Output()
	if err != nil {
		return fmt.Errorf("unable to get kernel version: %w", err)
	}

	ver, err := parseKernelVersion(strings.TrimSpace(string(unameOut)))
	if err != nil {
		return fmt.Errorf("unable to parse kernel version: %w", err)
	}

	modules := getKernelModules(ver)

	for _, mod := range modules {
		logger.Debugf("loading kernel module: %s", mod)
		if err := e.Command("modprobe", "--quiet", mod).Run(); err != nil {
			return fmt.Errorf("unable to load kernel module %s: %w", mod, err)
		}
	}

	var sb strings.Builder
	for _, mod := range modules {
		sb.WriteString(mod + "\n")
	}

	if err := f.Create(ctx, protonModulesLoadPath, false, []byte(sb.String())); err != nil {
		return fmt.Errorf("unable to write %s: %w", protonModulesLoadPath, err)
	}

	return nil
}
