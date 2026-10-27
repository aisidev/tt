//go:build !linux && !freebsd

package engine

import (
	"fmt"
	"github.com/aisidev/tt/model"
)

func platformEngine(kind model.Engine, containerRuntime *ContainerRuntime) (Engine, error) {
	return nil, fmt.Errorf("engine %s is not supported on this platform", kind)
}
