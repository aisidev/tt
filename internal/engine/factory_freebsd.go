package engine

import (
	"fmt"
	"github.com/aisidev/tt/model"
)

func platformEngine(kind model.Engine, containerRuntime *ContainerRuntime) (Engine, error) {
	switch kind {
	case model.Bhyve:
		return newBhyve(), nil
	case model.Jail:
		return newJail(), nil
	default:
		return nil, fmt.Errorf("engine %s is not supported on this platform", kind)
	}
}
