//go:build !linux

package guestconfig

import (
	"context"
	"fmt"
	"github.com/aisidev/tt/model"
)

func WriteDisk(ctx context.Context, directory string, files model.GuestConfig) error {
	return fmt.Errorf("guest configuration disks are not supported on this platform")
}
