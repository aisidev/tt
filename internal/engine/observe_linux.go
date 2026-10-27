//go:build linux

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/aisidev/tt/internal/netcfg"
	"github.com/aisidev/tt/model"
)

func readObservationFile(path string, limit int64) ([]byte, bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, false, fmt.Errorf("observation file exceeds its declared shape")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > limit {
		return nil, false, fmt.Errorf("observation file exceeds its byte budget")
	}
	return raw, true, nil
}
func (p processes) observePID(ctx context.Context, path string, markers []string) (uint32, error) {
	match := func(pid uint32) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		raw, found, err := readObservationFile(filepath.Join(p.root, strconv.FormatUint(uint64(pid), 10), "cmdline"), 64<<10)
		if err != nil || !found {
			return false, err
		}
		if len(raw) == 0 {
			stat, found, err := readObservationFile(filepath.Join(p.root, strconv.FormatUint(uint64(pid), 10), "stat"), 64<<10)
			if err != nil || !found {
				return false, err
			}
			index := bytes.LastIndex(stat, []byte(") "))
			if index >= 0 && bytes.HasPrefix(stat[index+2:], []byte("Z ")) {
				return false, nil
			}
			if index >= 0 {
				fields := strings.Fields(string(stat[index+2:]))
				if len(fields) > 6 {
					flags, e := strconv.ParseUint(fields[6], 10, 64)
					if e == nil && flags&0x00200000 != 0 {
						return false, nil
					}
				}
			}
			return false, fmt.Errorf("process identity unavailable")
		}
		for _, arg := range bytes.Split(raw, []byte{0}) {
			for _, marker := range markers {
				if string(arg) == marker {
					return true, nil
				}
			}
		}
		return false, nil
	}
	raw, found, err := readObservationFile(path, 128)
	if err != nil {
		return 0, err
	}
	if found {
		pid, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
		if err == nil && pid > 0 && pid <= math.MaxInt32 {
			yes, err := match(uint32(pid))
			if err != nil {
				return 0, err
			}
			if yes {
				return uint32(pid), nil
			}
		}
	}
	directory, err := os.Open(p.root)
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	count := 0
	var result uint32
	for {
		entries, e := directory.ReadDir(256)
		if e != nil && e != io.EOF {
			return 0, e
		}
		for _, entry := range entries {
			count++
			if count > maxProcessEntries {
				return 0, fmt.Errorf("process observation exceeds its entry budget")
			}
			pid, err := strconv.ParseUint(entry.Name(), 10, 32)
			if err != nil || pid == 0 || pid > math.MaxInt32 {
				continue
			}
			yes, err := match(uint32(pid))
			if err != nil {
				return 0, err
			}
			if yes {
				if result != 0 {
					return 0, fmt.Errorf("multiple process identities; observation unknown")
				}
				result = uint32(pid)
			}
		}
		if e == io.EOF {
			break
		}
	}
	return result, nil
}
func (q *Qemu) ObserveState(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	pid, err := q.processes.observePID(ctx, q.pidPath(vm), []string{q.pidPath(vm)})
	if err != nil {
		return "", err
	}
	if pid == 0 {
		return model.VmStopped, nil
	}
	status, err := q.monitor(ctx, vm, "info status")
	if err != nil {
		return "", err
	}
	if strings.Contains(status, "paused") {
		return model.VmPaused, nil
	}
	if strings.Contains(status, "running") {
		return model.VmRunning, nil
	}
	return "", fmt.Errorf("QEMU observation state unavailable")
}
func (f *Firecracker) ObserveState(ctx context.Context, vm *model.Vm) (model.VmState, error) {
	raw, found, err := readObservationFile(f.sandboxPath(vm), 4096)
	if err != nil {
		return "", err
	}
	var box sandbox
	markers := []string{f.legacySocket(vm), "/fc-" + netcfg.TapName(vm.ID) + ".sock"}
	if found {
		if json.Unmarshal(raw, &box) != nil {
			return "", fmt.Errorf("sandbox observation malformed")
		}
		markers = append(markers, box.Marker)
	}
	pid, err := f.processes.observePID(ctx, f.pidPath(vm), markers)
	if err != nil {
		return "", err
	}
	if pid == 0 {
		return model.VmStopped, nil
	}
	socket := f.legacySocket(vm)
	if found {
		socket = box.socket()
	} else {
		yes, err := f.processes.matches(pid, "/fc-"+netcfg.TapName(vm.ID)+".sock")
		if err != nil {
			return "", err
		}
		if yes {
			return "", fmt.Errorf("owned sandbox metadata unavailable")
		}
	}
	data, err := firecrackerRequest(ctx, socket, "GET", "/", nil)
	if err != nil {
		return "", err
	}
	var state struct {
		State string `json:"state"`
	}
	if json.Unmarshal(data, &state) != nil {
		return "", fmt.Errorf("Firecracker observation malformed")
	}
	switch state.State {
	case "Running":
		return model.VmRunning, nil
	case "Paused":
		return model.VmPaused, nil
	case "Not started":
		return model.VmCreating, nil
	}
	return "", fmt.Errorf("Firecracker observation state unavailable")
}
