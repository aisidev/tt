package guestconfig

import (
	"context"
	"github.com/aisidev/tt/model"
)

func WriteDisk(ctx context.Context, directory string, files model.GuestConfig) error {
	return writeDisk(ctx, directory, files)
}
