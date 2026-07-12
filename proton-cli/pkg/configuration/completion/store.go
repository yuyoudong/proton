package completion

import (
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/configuration"
	"devops.aishu.cn/AISHUDevOps/ICT/_git/proton-opensource.git/proton-cli/v3/pkg/proton/store"
)

func CompletePackageStore(c *configuration.PackageStore) {
	if len(c.Hosts) != 0 {
		CompleteBaredPackageStore(c)
	} else {
		CompleteHostedPackageStore(c)
	}
}

func CompleteBaredPackageStore(c *configuration.PackageStore) {
	if c.Replicas == nil {
		c.Replicas = ptr.To(len(c.Hosts))
	}
	if c.Storage.Capacity == nil {
		c.Storage.Capacity = resource.NewQuantity(store.DefaultStorageCapacity, resource.BinarySI)
	}
	if c.Storage.Path == "" {
		c.Storage.Path = store.DefaultStoragePath
	}
}

func CompleteHostedPackageStore(c *configuration.PackageStore) {
	if c.Replicas == nil {
		c.Replicas = ptr.To(store.DefaultReplicas)
	}
	if c.Storage.Capacity == nil {
		c.Storage.Capacity = resource.NewQuantity(store.DefaultStorageCapacity, resource.BinarySI)
	}
}
