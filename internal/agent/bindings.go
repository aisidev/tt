package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/aisidev/tt/model"
)

func (d *database) bindNetwork(ctx context.Context, ingress *SshIngress, start, end uint16) error {
	if start == 0 || start > end {
		return fmt.Errorf("host port range must satisfy 1 <= port-start <= port-end <= 65535")
	}
	if ingress != nil {
		if err := ingress.Validate(); err != nil {
			return err
		}
	}
	encoded, err := model.SerdeJSON(map[string]any{"ingress": ingress, "port_start": start, "port_end": end})
	if err != nil {
		return err
	}
	requested, err := model.DecodeJSONValue(encoded)
	if err != nil {
		return err
	}
	var stored sqlText
	err = d.conn.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key='network_config'").Scan(&stored)
	present := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read network binding: %w", err)
	}
	var occupied bool
	if err := d.conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM vms)").Scan(&occupied); err != nil {
		return fmt.Errorf("check network inventory: %w", err)
	}
	if present {
		saved, err := model.DecodeJSONValue([]byte(stored))
		if err != nil {
			return fmt.Errorf("decode network binding: %w", err)
		}
		if occupied && !reflect.DeepEqual(saved, requested) {
			return fmt.Errorf("cannot change SSH ingress or host port range while VM records remain; restore the saved configuration and delete those VMs first")
		}
	} else if occupied {
		return fmt.Errorf("agent database has VM records but no network binding; inspect the database before starting")
	}
	if _, err := d.conn.ExecContext(ctx, "INSERT OR REPLACE INTO _meta (key,value) VALUES ('network_config',?1)", string(encoded)); err != nil {
		return fmt.Errorf("persist network binding: %w", err)
	}
	return nil
}
func allocatePorts(vms []model.Vm, ports []uint16, start, end uint16, available func(uint16) bool) (model.Map[uint16, uint16], error) {
	used := map[uint16]bool{}
	for _, vm := range vms {
		for _, host := range vm.PortMap {
			used[host] = true
		}
	}
	result := model.Map[uint16, uint16]{}
	next := uint32(start)
	for _, guest := range ports {
		if _, ok := result[guest]; ok {
			continue
		}
		found := false
		for next <= uint32(end) {
			host := uint16(next)
			next++
			if !used[host] && available(host) {
				result[guest] = host
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("host TCP port pool exhausted")
		}
	}
	return result, nil
}
