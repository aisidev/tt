package engine

import (
	"fmt"

	"github.com/aisidev/tt/model"
)

func platformEngine(kind model.Engine, containerRuntime *ContainerRuntime) (Engine, error) {
	switch kind {
	case model.Qemu:
		return newQemu(), nil
	case model.Firecracker:
		return newFirecracker(), nil
	case model.Docker:
		if containerRuntime == nil {
			return nil, fmt.Errorf("container runtime is not bound")
		}
		if _, err := ParseContainerRuntime(string(*containerRuntime)); err != nil {
			return nil, err
		}
		return &Docker{runtime: *containerRuntime}, nil
	default:
		return nil, fmt.Errorf("engine %s is not supported on this platform", kind)
	}
}
